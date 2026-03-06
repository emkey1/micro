package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-errors/errors"
	isatty "github.com/mattn/go-isatty"
	"github.com/micro-editor/micro/v2/internal/action"
	"github.com/micro-editor/micro/v2/internal/buffer"
	"github.com/micro-editor/micro/v2/internal/clipboard"
	"github.com/micro-editor/micro/v2/internal/config"
	"github.com/micro-editor/micro/v2/internal/screen"
	"github.com/micro-editor/micro/v2/internal/shell"
	"github.com/micro-editor/micro/v2/internal/util"
	"github.com/micro-editor/tcell/v2"
	lua "github.com/yuin/gopher-lua"
)

var (
	// Command line flags
	flagVersion   *bool
	flagConfigDir *string
	flagOptions   *bool
	flagDebug     *bool
	flagProfile   *bool
	flagPlugin    *string
	flagClean     *bool
	optionFlags   map[string]*string

	timerChan chan func()
)

type pscalRuntimeState struct {
	sessionID       uint64
	sighup          chan os.Signal
	sigterm         chan os.Signal
	eventPollStop   chan struct{}
	eventPollDone   chan struct{}
	resizeWatchStop chan struct{}
	resizeWatchDone chan struct{}
	embeddedMode    bool
	lastResizeCols  int
	lastResizeRows  int
	screen          tcell.Screen
	events          chan tcell.Event
	drawChan        chan bool
	timer           chan func()
	jobs            chan shell.JobFunction
	closeTerms      chan bool
	tabs            *action.TabList
	infoBar         *action.InfoPane
	logBufPane      *action.BufPane
	openBuffers     []*buffer.Buffer
	logBuf          *buffer.Buffer
	tcellInFD       int
	tcellOutFD      int
}

var pscalScreenBindMu sync.Mutex

func pscalBindRuntimeScreenLocked(rt *pscalRuntimeState) {
	if rt == nil {
		return
	}
	if rt.screen != nil {
		screen.Screen = rt.screen
	}
	if rt.events != nil {
		screen.Events = rt.events
	}
}

func pscalBindRuntimeStateLocked(rt *pscalRuntimeState) {
	if rt == nil {
		return
	}
	pscalBindRuntimeScreenLocked(rt)
	if rt.timer != nil {
		timerChan = rt.timer
	}
	if rt.jobs != nil {
		shell.Jobs = rt.jobs
	}
	if rt.closeTerms != nil {
		shell.CloseTerms = rt.closeTerms
	}
	if rt.sigterm != nil {
		util.Sigterm = rt.sigterm
	}
	if rt.tabs != nil {
		action.Tabs = rt.tabs
	}
	if rt.infoBar != nil {
		action.InfoBar = rt.infoBar
		buffer.SetMessager(rt.infoBar)
	}
	action.LogBufPane = rt.logBufPane
	buffer.OpenBuffers = rt.openBuffers
	buffer.LogBuf = rt.logBuf
}

func pscalCaptureRuntimeStateLocked(rt *pscalRuntimeState) {
	if rt == nil {
		return
	}
	rt.tabs = action.Tabs
	rt.infoBar = action.InfoBar
	rt.logBufPane = action.LogBufPane
	rt.openBuffers = buffer.OpenBuffers
	rt.logBuf = buffer.LogBuf
	rt.timer = timerChan
	rt.jobs = shell.Jobs
	rt.closeTerms = shell.CloseTerms
	rt.sigterm = util.Sigterm
}

func pscalEnvResizePollingEnabled(rt *pscalRuntimeState) bool {
	if !pscalIsEmbeddedRuntime(rt) {
		return false
	}
	value := strings.TrimSpace(pscalLookupEnv("PSCAL_MICRO_ENV_RESIZE_POLL"))
	if value == "" {
		// Embedded bridge now posts per-session resize events directly.
		// Default polling off to avoid shared env churn across runtimes.
		return false
	}
	return value != "0"
}

type pscalExitPanic struct {
	code int
}

func pscalIsEmbeddedRuntime(rt *pscalRuntimeState) bool {
	if rt != nil && rt.embeddedMode {
		return true
	}
	return pscalLookupEnv("PSCAL_MICRO_EMBEDDED") == "1"
}

func pscalParseEnvSize() (int, int, bool) {
	parse := func(name string, fallback int) int {
		value := strings.TrimSpace(pscalLookupEnv(name))
		if value == "" {
			return fallback
		}
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed <= 0 || parsed > 1000 {
			return fallback
		}
		return parsed
	}
	cols := parse("COLUMNS", 0)
	rows := parse("LINES", 0)
	if cols <= 0 || rows <= 0 {
		return 0, 0, false
	}
	return cols, rows, true
}

func pscalSyncGoEnvSizeFromC() (int, int, bool) {
	cols, rows, ok := pscalParseEnvSize()
	if !ok {
		return 0, 0, false
	}
	_ = os.Setenv("COLUMNS", strconv.Itoa(cols))
	_ = os.Setenv("LINES", strconv.Itoa(rows))
	return cols, rows, true
}

func pscalPostResizeFromEnv(rt *pscalRuntimeState, force bool) {
	if !pscalIsEmbeddedRuntime(rt) || rt == nil || rt.screen == nil {
		return
	}
	cols, rows, ok := pscalSyncGoEnvSizeFromC()
	if !ok {
		return
	}
	if !force && rt != nil && cols == rt.lastResizeCols && rows == rt.lastResizeRows {
		return
	}
	if rt != nil {
		rt.lastResizeCols = cols
		rt.lastResizeRows = rows
	}
	_ = rt.screen.PostEvent(tcell.NewEventResize(cols, rows))
}

func pscalExitCodeFromRecovered(v interface{}) (int, bool) {
	switch x := v.(type) {
	case pscalExitPanic:
		return x.code, true
	case *pscalExitPanic:
		if x != nil {
			return x.code, true
		}
	}

	text := strings.TrimSpace(fmt.Sprint(v))
	if text == "" {
		return 0, false
	}
	if strings.HasPrefix(text, "{") && strings.HasSuffix(text, "}") && len(text) >= 2 {
		text = strings.TrimSpace(text[1 : len(text)-1])
	}
	code, err := strconv.Atoi(text)
	if err != nil {
		return 0, false
	}
	return code, true
}

func InitFlags() {
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	flag.CommandLine.SetOutput(io.Discard)
	flagVersion = flag.Bool("version", false, "Show the version number and information")
	flagConfigDir = flag.String("config-dir", "", "Specify a custom location for the configuration directory")
	flagOptions = flag.Bool("options", false, "Show all option help")
	flagDebug = flag.Bool("debug", false, "Enable debug mode (prints debug info to ./log.txt)")
	flagProfile = flag.Bool("profile", false, "Enable CPU profiling (writes profile info to ./micro.prof)")
	flagPlugin = flag.String("plugin", "", "Plugin command")
	flagClean = flag.Bool("clean", false, "Clean configuration directory")
	// Note: keep this in sync with the man page in assets/packaging/micro.1
	flag.Usage = func() {
		fmt.Println("Usage: micro [OPTION]... [FILE]... [+LINE[:COL]] [+/REGEX]")
		fmt.Println("       micro [OPTION]... [FILE[:LINE[:COL]]]...  (only if the `parsecursor` option is enabled)")
		fmt.Println("-clean")
		fmt.Println("    \tClean the configuration directory and exit")
		fmt.Println("-config-dir dir")
		fmt.Println("    \tSpecify a custom location for the configuration directory")
		fmt.Println("FILE:LINE[:COL] (only if the `parsecursor` option is enabled)")
		fmt.Println("FILE +LINE[:COL]")
		fmt.Println("    \tSpecify a line and column to start the cursor at when opening a buffer")
		fmt.Println("+/REGEX")
		fmt.Println("    \tSpecify a regex to search for when opening a buffer")
		fmt.Println("-options")
		fmt.Println("    \tShow all options help and exit")
		fmt.Println("-debug")
		fmt.Println("    \tEnable debug mode (enables logging to ./log.txt)")
		fmt.Println("-profile")
		fmt.Println("    \tEnable CPU profiling (writes profile info to ./micro.prof")
		fmt.Println("    \tso it can be analyzed later with \"go tool pprof micro.prof\")")
		fmt.Println("-version")
		fmt.Println("    \tShow the version number and information and exit")

		fmt.Print("\nMicro's plugins can be managed at the command line with the following commands.\n")
		fmt.Println("-plugin install [PLUGIN]...")
		fmt.Println("    \tInstall plugin(s)")
		fmt.Println("-plugin remove [PLUGIN]...")
		fmt.Println("    \tRemove plugin(s)")
		fmt.Println("-plugin update [PLUGIN]...")
		fmt.Println("    \tUpdate plugin(s) (if no argument is given, updates all plugins)")
		fmt.Println("-plugin search [PLUGIN]...")
		fmt.Println("    \tSearch for a plugin")
		fmt.Println("-plugin list")
		fmt.Println("    \tList installed plugins")
		fmt.Println("-plugin available")
		fmt.Println("    \tList available plugins")

		fmt.Print("\nMicro's options can also be set via command line arguments for quick\nadjustments. For real configuration, please use the settings.json\nfile (see 'help options').\n\n")
		fmt.Println("-<option> value")
		fmt.Println("    \tSet `option` to `value` for this session")
		fmt.Println("    \tFor example: `micro -syntax off file.c`")
		fmt.Println("\nUse `micro -options` to see the full list of configuration options")
	}

	optionFlags = make(map[string]*string)

	for k, v := range config.DefaultAllSettings() {
		optionFlags[k] = flag.String(k, "", fmt.Sprintf("The %s option. Default value: '%v'.", k, v))
	}

	flag.Parse()

	if *flagVersion {
		// If -version was passed
		fmt.Println("Version:", util.Version)
		fmt.Println("Commit hash:", util.CommitHash)
		fmt.Println("Compiled on", util.CompileDate)
		exit(0)
	}

	if *flagOptions {
		// If -options was passed
		var keys []string
		m := config.DefaultAllSettings()
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := m[k]
			fmt.Printf("-%s value\n", k)
			fmt.Printf("    \tDefault value: '%v'\n", v)
		}
		exit(0)
	}

	if util.Debug == "OFF" && *flagDebug {
		util.Debug = "ON"
	}
}

// DoPluginFlags parses and executes any flags that require LoadAllPlugins (-plugin and -clean)
func DoPluginFlags() {
	if *flagClean || *flagPlugin != "" {
		config.LoadAllPlugins()

		if *flagPlugin != "" {
			args := flag.Args()

			config.PluginCommand(os.Stdout, *flagPlugin, args)
		} else if *flagClean {
			CleanConfig()
		}

		exit(0)
	}
}

// LoadInput determines which files should be loaded into buffers
// based on the input stored in flag.Args()
func LoadInput(args []string) []*buffer.Buffer {
	// There are a number of ways micro should start given its input

	// 1. If it is given a files in flag.Args(), it should open those

	// 2. If there is no input file and the input is not a terminal, that means
	// something is being piped in and the stdin should be opened in an
	// empty buffer

	// 3. If there is no input file and the input is a terminal, an empty buffer
	// should be opened

	buffers := make([]*buffer.Buffer, 0, len(args))

	files := make([]string, 0, len(args))

	flagStartPos := buffer.Loc{-1, -1}
	posFlagr := regexp.MustCompile(`^\+(\d+)(?::(\d+))?$`)
	posIndex := -1

	searchText := ""
	searchFlagr := regexp.MustCompile(`^\+\/(.+)$`)
	searchIndex := -1

	for i, a := range args {
		posMatch := posFlagr.FindStringSubmatch(a)
		if len(posMatch) == 3 && posMatch[2] != "" {
			line, err := strconv.Atoi(posMatch[1])
			if err != nil {
				screen.TermMessage(err)
				continue
			}
			col, err := strconv.Atoi(posMatch[2])
			if err != nil {
				screen.TermMessage(err)
				continue
			}
			flagStartPos = buffer.Loc{col - 1, line - 1}
			posIndex = i
		} else if len(posMatch) == 3 && posMatch[2] == "" {
			line, err := strconv.Atoi(posMatch[1])
			if err != nil {
				screen.TermMessage(err)
				continue
			}
			flagStartPos = buffer.Loc{0, line - 1}
			posIndex = i
		} else {
			searchMatch := searchFlagr.FindStringSubmatch(a)
			if len(searchMatch) == 2 {
				searchText = searchMatch[1]
				searchIndex = i
			} else {
				files = append(files, a)
			}
		}
	}

	command := buffer.Command{
		StartCursor:      flagStartPos,
		SearchRegex:      searchText,
		SearchAfterStart: searchIndex > posIndex,
	}

	if len(files) > 0 {
		// Option 1
		// We go through each file and load it
		for i := 0; i < len(files); i++ {
			buf, err := buffer.NewBufferFromFileWithCommand(files[i], buffer.BTDefault, command)
			if err != nil {
				screen.TermMessage(err)
				continue
			}
			// If the file didn't exist, input will be empty, and we'll open an empty buffer
			buffers = append(buffers, buf)
		}
	} else {
		btype := buffer.BTDefault
		embedded := pscalIsEmbeddedRuntime(nil)
		if !embedded && !isatty.IsTerminal(os.Stdout.Fd()) {
			btype = buffer.BTStdout
		}

		if !embedded && !isatty.IsTerminal(os.Stdin.Fd()) {
			// Option 2
			// The input is not a terminal, so something is being piped in
			// and we should read from stdin
			input, err := io.ReadAll(os.Stdin)
			if err != nil {
				screen.TermMessage("Error reading from stdin: ", err)
				input = []byte{}
			}
			buffers = append(buffers, buffer.NewBufferFromStringWithCommand(string(input), "", btype, command))
		} else {
			// Option 3, just open an empty buffer
			buffers = append(buffers, buffer.NewBufferFromStringWithCommand("", "", btype, command))
		}
	}

	return buffers
}

func checkBackup(name string) error {
	target := filepath.Join(config.ConfigDir, name)
	backup := target + util.BackupSuffix
	if info, err := os.Stat(backup); err == nil {
		input, err := os.ReadFile(backup)
		if err == nil {
			t := info.ModTime()
			msg := fmt.Sprintf(buffer.BackupMsg, target, t.Format("Mon Jan _2 at 15:04, 2006"), backup)
			choice := screen.TermPrompt(msg, []string{"r", "i", "a", "recover", "ignore", "abort"}, true)

			if choice%3 == 0 {
				// recover
				err := os.WriteFile(target, input, util.FileMode)
				if err != nil {
					return err
				}
				return os.Remove(backup)
			} else if choice%3 == 1 {
				// delete
				return os.Remove(backup)
			} else if choice%3 == 2 {
				// abort
				return errors.New("Aborted")
			}
		}
	}
	return nil
}

func exit(rc int) {
	exitWithRuntime(nil, rc)
}

func exitWithRuntime(rt *pscalRuntimeState, rc int) {
	config.StopAutoSave()
	if rt != nil && rt.sigterm != nil {
		signal.Stop(rt.sigterm)
	}
	if rt != nil && rt.sighup != nil {
		signal.Stop(rt.sighup)
	}
	var runtimeScreen tcell.Screen
	if rt != nil {
		runtimeScreen = rt.screen
	}
	pscalStopResizeWatcher(rt, runtimeScreen)
	pscalStopEventPoller(rt, runtimeScreen)

	for _, b := range buffer.OpenBuffers {
		if !b.Modified() {
			b.Fini()
		}
	}

	if runtimeScreen != nil {
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Println("Warning: ignored panic during screen shutdown:", r)
				}
			}()
			runtimeScreen.Fini()
		}()
	}

	if pscalIsEmbeddedRuntime(rt) {
		panic(pscalExitPanic{code: rc})
	}

	os.Exit(rc)
}

func pscalStopEventPoller(rt *pscalRuntimeState, scr tcell.Screen) {
	if rt == nil {
		return
	}
	stop := rt.eventPollStop
	done := rt.eventPollDone
	rt.eventPollStop = nil
	rt.eventPollDone = nil

	if stop != nil {
		close(stop)
	}
	if scr != nil {
		func() {
			defer func() {
				_ = recover()
			}()
			_ = scr.PostEvent(tcell.NewEventResize(1, 1))
		}()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func pscalStartResizeWatcher(rt *pscalRuntimeState, scr tcell.Screen) {
	if rt == nil || !pscalIsEmbeddedRuntime(rt) || !pscalEnvResizePollingEnabled(rt) || scr == nil || rt.resizeWatchStop != nil {
		return
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	rt.resizeWatchStop = stop
	rt.resizeWatchDone = done
	go func() {
		defer close(done)
		ticker := time.NewTicker(75 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				pscalPostResizeFromEnv(rt, false)
			}
		}
	}()
}

func pscalStopResizeWatcher(rt *pscalRuntimeState, scr tcell.Screen) {
	if rt == nil {
		return
	}
	stop := rt.resizeWatchStop
	done := rt.resizeWatchDone
	rt.resizeWatchStop = nil
	rt.resizeWatchDone = nil

	if stop != nil {
		close(stop)
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func pscalMicroMain(rt *pscalRuntimeState) {
	if rt == nil {
		rt = &pscalRuntimeState{}
	}
	if pscalIsEmbeddedRuntime(rt) {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
	}
	defer func() {
		if util.Stdout.Len() > 0 {
			fmt.Fprint(os.Stdout, util.Stdout.String())
		}
		if r := recover(); r != nil {
			panic(r)
		}
		exitWithRuntime(rt, 0)
	}()

	var err error

	InitFlags()

	if *flagProfile {
		f, err := os.Create("micro.prof")
		if err != nil {
			log.Fatal("error creating CPU profile: ", err)
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			log.Fatal("error starting CPU profile: ", err)
		}
		defer pprof.StopCPUProfile()
	}

	InitLog()

	err = config.InitConfigDir(*flagConfigDir)
	if err != nil {
		screen.TermMessage(err)
		exitWithRuntime(rt, 1)
	}

	config.InitRuntimeFiles(true)
	config.InitPlugins()

	err = checkBackup("settings.json")
	if err != nil {
		screen.TermMessage(err)
		exitWithRuntime(rt, 1)
	}

	err = config.ReadSettings()
	if err != nil {
		screen.TermMessage(err)
	}
	err = config.InitGlobalSettings()
	if err != nil {
		screen.TermMessage(err)
	}

	// flag options
	for k, v := range optionFlags {
		if *v != "" {
			nativeValue, err := config.GetNativeValue(k, *v)
			if err != nil {
				screen.TermMessage(err)
				continue
			}
			if err = config.OptionIsValid(k, nativeValue); err != nil {
				screen.TermMessage(err)
				continue
			}
			config.GlobalSettings[k] = nativeValue
			config.VolatileSettings[k] = true
		}
	}

	DoPluginFlags()

	restoreTcellProvider := pscalInstallTcellStdioProvider(rt)
	defer restoreTcellProvider()
	err = screen.Init()
	if err != nil {
		fmt.Println(err)
		fmt.Println("Fatal: Micro could not initialize a Screen.")
		exitWithRuntime(rt, 1)
	}
	rt.screen = screen.Screen
	rt.drawChan = screen.DrawChan()
	m := clipboard.SetMethod(config.GetGlobalOption("clipboard").(string))
	clipErr := clipboard.Initialize(m)

	defer func() {
		if err := recover(); err != nil {
			if pscalIsEmbeddedRuntime(rt) {
				if _, ok := pscalExitCodeFromRecovered(err); ok {
					panic(err)
				}
			}
			if rt.screen != nil {
				rt.screen.Fini()
			}
			if e, ok := err.(*lua.ApiError); ok {
				fmt.Println("Lua API error:", e)
			} else {
				fmt.Println("Micro encountered an error:", errors.Wrap(err, 2).ErrorStack(), "\nIf you can reproduce this error, please report it at https://github.com/micro-editor/micro/issues")
			}
			// immediately backup all buffers with unsaved changes
			for _, b := range buffer.OpenBuffers {
				if b.Modified() {
					b.Backup()
				}
			}
			exitWithRuntime(rt, 1)
		}
	}()

	err = config.LoadAllPlugins()
	if err != nil {
		screen.TermMessage(err)
	}

	err = checkBackup("bindings.json")
	if err != nil {
		screen.TermMessage(err)
		exitWithRuntime(rt, 1)
	}

	action.InitBindings()
	action.InitCommands()
	action.SetQuitFunc(exit)

	err = config.RunPluginFn("preinit")
	if err != nil {
		screen.TermMessage(err)
	}

	action.InitGlobals()
	buffer.SetMessager(action.InfoBar)
	args := flag.Args()
	b := LoadInput(args)

	if len(b) == 0 {
		// No buffers to open
		if rt.screen != nil {
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Println("Warning: ignored panic during screen shutdown:", r)
					}
				}()
				rt.screen.Fini()
			}()
		}
		exitWithRuntime(rt, 0)
	}

	action.InitTabs(b)
	func() {
		pscalScreenBindMu.Lock()
		defer pscalScreenBindMu.Unlock()
		pscalCaptureRuntimeStateLocked(rt)
	}()

	err = config.RunPluginFn("init")
	if err != nil {
		screen.TermMessage(err)
	}

	err = config.RunPluginFn("postinit")
	if err != nil {
		screen.TermMessage(err)
	}

	err = config.InitColorscheme()
	if err != nil {
		screen.TermMessage(err)
	}

	if clipErr != nil {
		log.Println(clipErr, " or change 'clipboard' option")
	}

	config.StartAutoSave()
	if a := config.GetGlobalOption("autosave").(float64); a > 0 {
		config.SetAutoTime(a)
	}

	rt.events = make(chan tcell.Event)
	screen.Events = rt.events
	eventsCh := rt.events

	if rt.sigterm == nil {
		rt.sigterm = make(chan os.Signal, 1)
	}
	rt.sighup = make(chan os.Signal, 1)
	signal.Notify(rt.sigterm, syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT)
	signal.Notify(rt.sighup, syscall.SIGHUP)

	if rt.timer == nil {
		rt.timer = make(chan func())
	}
	timerChan = rt.timer
	if rt.jobs == nil {
		rt.jobs = make(chan shell.JobFunction, 100)
	}
	shell.Jobs = rt.jobs
	if rt.closeTerms == nil {
		rt.closeTerms = make(chan bool)
	}
	shell.CloseTerms = rt.closeTerms
	pollStop := make(chan struct{})
	pollDone := make(chan struct{})
	rt.eventPollStop = pollStop
	rt.eventPollDone = pollDone

	// Here is the event loop which runs in a separate thread
	go func(scr tcell.Screen, events chan tcell.Event, stop <-chan struct{}, done chan<- struct{}) {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				log.Println("Warning: ignored panic in event poll loop:", r)
			}
		}()
		embedded := pscalIsEmbeddedRuntime(rt)
		for {
			select {
			case <-stop:
				return
			default:
			}
			var e tcell.Event
			if embedded {
				// screen.Lock() is process-global in micro. Holding it while an
				// unfocused runtime blocks in PollEvent can starve other embedded
				// runtimes in the same process.
				e = scr.PollEvent()
			} else {
				screen.Lock()
				e = scr.PollEvent()
				screen.Unlock()
			}
			select {
			case <-stop:
				return
			default:
			}
			if e == nil {
				if pscalIsEmbeddedRuntime(rt) {
					return
				}
				continue
			}
			select {
			case events <- e:
			case <-stop:
				return
			}
		}
	}(rt.screen, eventsCh, pollStop, pollDone)

	// clear the drawchan so we don't redraw excessively
	// if someone requested a redraw before we started displaying
	for rt.drawChan != nil && len(rt.drawChan) > 0 {
		<-rt.drawChan
	}

	// In embedded/iOS mode we may run over stdio relays where terminal-driven
	// resize events are unreliable. Seed an explicit initial resize from
	// COLUMNS/LINES when available.
	pscalPostResizeFromEnv(rt, true)
	pscalStartResizeWatcher(rt, rt.screen)

	// wait for initial resize event
	select {
	case event := <-rt.events:
		func() {
			pscalScreenBindMu.Lock()
			defer pscalScreenBindMu.Unlock()
			pscalBindRuntimeStateLocked(rt)
			defer pscalCaptureRuntimeStateLocked(rt)
			action.Tabs.HandleEvent(event)
		}()
	case <-time.After(10 * time.Millisecond):
		// time out after 10ms
	}

	for {
		DoEvent(rt)
	}
}

func main() {
	pscalMicroMain(&pscalRuntimeState{})
}

// DoEvent runs the main action loop of the editor
func DoEvent(rt *pscalRuntimeState) {
	var event tcell.Event
	var jobFunc *shell.JobFunction
	var timerFunc func()
	runAutosave := false
	runCloseTerms := false

	// In embedded/iOS mode, bridge code updates COLUMNS/LINES for the active
	// session and posts SIGWINCH. Polling keeps micro aligned even when terminal
	// ioctls are unavailable (pipe relay mode).
	if pscalEnvResizePollingEnabled(rt) {
		pscalPostResizeFromEnv(rt, false)
	}

	// Display everything
	func() {
		pscalScreenBindMu.Lock()
		defer pscalScreenBindMu.Unlock()
		pscalBindRuntimeStateLocked(rt)
		defer pscalCaptureRuntimeStateLocked(rt)
		if rt != nil && rt.screen != nil {
			rt.screen.Fill(' ', config.DefStyle)
			rt.screen.HideCursor()
			action.Tabs.Display()
			for _, ep := range action.MainTab().Panes {
				ep.Display()
			}
			action.MainTab().Display()
			action.InfoBar.Display()
			rt.screen.Show()
		}
	}()

	// Check for new events
	select {
	case f := <-rt.jobs:
		// If a new job has finished while running in the background we should execute the callback
		jobFunc = &f
	case <-config.Autosave:
		runAutosave = true
	case <-rt.closeTerms:
		runCloseTerms = true
	case event = <-rt.events:
	case <-rt.drawChan:
		for len(rt.drawChan) > 0 {
			<-rt.drawChan
		}
	case f := <-rt.timer:
		timerFunc = f
	case <-rt.sighup:
		exitWithRuntime(rt, 0)
	case <-rt.sigterm:
		exitWithRuntime(rt, 0)
	}

	func() {
		pscalScreenBindMu.Lock()
		defer pscalScreenBindMu.Unlock()
		pscalBindRuntimeStateLocked(rt)
		defer pscalCaptureRuntimeStateLocked(rt)
		if jobFunc != nil {
			jobFunc.Function(jobFunc.Output, jobFunc.Args)
		}
		if runAutosave {
			for _, b := range buffer.OpenBuffers {
				b.AutoSave()
			}
		}
		if runCloseTerms {
			action.Tabs.CloseTerms()
		}
		if timerFunc != nil {
			timerFunc()
		}

		if e, ok := event.(*tcell.EventError); ok {
			log.Println("tcell event error: ", e.Error())

			if e.Err() == io.EOF {
				// In embedded mode EOF may be transient while the host PTY bridge
				// settles; keep the editor loop alive instead of hard-exiting.
				if pscalIsEmbeddedRuntime(rt) {
					time.Sleep(10 * time.Millisecond)
					return
				}
				// shutdown due to terminal closing/becoming inaccessible
				exitWithRuntime(rt, 0)
			}
			return
		}

		if event != nil {
			_, resize := event.(*tcell.EventResize)
			if resize {
				action.InfoBar.HandleEvent(event)
				action.Tabs.HandleEvent(event)
			} else if action.InfoBar.HasPrompt {
				action.InfoBar.HandleEvent(event)
			} else {
				action.Tabs.HandleEvent(event)
			}
		}

		err := config.RunPluginFn("onAnyEvent")
		if err != nil {
			screen.TermMessage(err)
		}
	}()
}
