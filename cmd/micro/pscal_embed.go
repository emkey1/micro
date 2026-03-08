//go:build pscal_embed

package main

/*
#include <stdint.h>

__attribute__((weak))
uint64_t pscal_micro_current_session_id(void) {
	return 0;
}

__attribute__((weak))
int pscal_micro_current_stdio_fds(int *stdin_fd, int *stdout_fd) {
	if (stdin_fd) {
		*stdin_fd = -1;
	}
	if (stdout_fd) {
		*stdout_fd = -1;
	}
	return 0;
}
*/
import "C"

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"unsafe"

	"github.com/micro-editor/tcell/v2"
)

var pscalRuntimeRegistryMu sync.Mutex
var pscalRuntimeRegistry = map[uint64]*pscalRuntimeState{}

func pscalSessionIDFromHost() uint64 {
	return uint64(C.pscal_micro_current_session_id())
}

func pscalCurrentStdioFDsFromHost() (int, int, bool) {
	var stdinFD C.int
	var stdoutFD C.int
	if C.pscal_micro_current_stdio_fds(&stdinFD, &stdoutFD) == 0 {
		return 0, 0, false
	}
	if stdinFD < 0 || stdoutFD < 0 {
		return 0, 0, false
	}
	return int(stdinFD), int(stdoutFD), true
}

func pscalPopulateRuntimeLaunchContext(rt *pscalRuntimeState) {
	if rt == nil {
		return
	}
	if sessionID := pscalSessionIDFromHost(); sessionID != 0 {
		rt.sessionID = sessionID
	} else {
		rt.sessionID = pscalSessionIDFromEnv()
	}
	if stdinFD, stdoutFD, ok := pscalCurrentStdioFDsFromHost(); ok {
		rt.tcellInFD = stdinFD
		rt.tcellOutFD = stdoutFD
	} else {
		rt.tcellInFD = -1
		rt.tcellOutFD = -1
	}
}

func pscalSessionIDFromEnv() uint64 {
	value := strings.TrimSpace(os.Getenv("PSCAL_MICRO_SESSION_ID"))
	if value == "" {
		return 0
	}
	id, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0
	}
	return id
}

func pscalRegisterRuntime(rt *pscalRuntimeState) {
	if rt == nil || rt.sessionID == 0 {
		return
	}
	pscalRuntimeRegistryMu.Lock()
	pscalRuntimeRegistry[rt.sessionID] = rt
	pscalRuntimeRegistryMu.Unlock()
}

func pscalUnregisterRuntime(rt *pscalRuntimeState) {
	if rt == nil || rt.sessionID == 0 {
		return
	}
	pscalRuntimeRegistryMu.Lock()
	current, ok := pscalRuntimeRegistry[rt.sessionID]
	if ok && current == rt {
		delete(pscalRuntimeRegistry, rt.sessionID)
	}
	pscalRuntimeRegistryMu.Unlock()
}

func pscalPostRuntimeResize(sessionID uint64, cols, rows int) bool {
	if sessionID == 0 || cols <= 0 || rows <= 0 {
		return false
	}
	pscalRuntimeRegistryMu.Lock()
	rt := pscalRuntimeRegistry[sessionID]
	pscalRuntimeRegistryMu.Unlock()
	if rt == nil || rt.screen == nil {
		return false
	}
	rt.lastResizeCols = cols
	rt.lastResizeRows = rows
	defer func() {
		_ = recover()
	}()
	if applier, ok := rt.screen.(interface{ PSCALApplyResize(int, int) }); ok {
		applier.PSCALApplyResize(cols, rows)
		return true
	}
	return rt.screen.PostEvent(tcell.NewEventResize(cols, rows)) == nil
}

func pscalHasConfigDirFlag(args []string) bool {
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "-config-dir" {
			return true
		}
		if strings.HasPrefix(arg, "-config-dir=") {
			return true
		}
	}
	return false
}

func pscalResolveWorkdir() string {
	workdir := os.Getenv("PSCALI_WORKDIR")
	if workdir != "" {
		return workdir
	}
	containerRoot := os.Getenv("PSCALI_CONTAINER_ROOT")
	if containerRoot != "" {
		return filepath.Join(containerRoot, "Documents", "home")
	}
	home := os.Getenv("HOME")
	if home != "" {
		return home
	}
	return ""
}

func pscalPrepareConfigEnv() (configDir string) {
	workdir := pscalResolveWorkdir()
	if workdir == "" {
		return ""
	}
	xdgHome := filepath.Join(workdir, ".config")
	configDir = filepath.Join(xdgHome, "micro")
	_ = os.Setenv("PSCALI_WORKDIR", workdir)
	_ = os.Setenv("HOME", workdir)
	_ = os.Setenv("XDG_CONFIG_HOME", xdgHome)
	_ = os.Setenv("MICRO_CONFIG_HOME", configDir)
	_ = os.MkdirAll(configDir, 0o700)
	return configDir
}

func pscalInjectConfigDir(args []string, configDir string) []string {
	if len(args) == 0 || pscalHasConfigDirFlag(args) || configDir == "" {
		return args
	}
	patched := make([]string, 0, len(args)+2)
	patched = append(patched, args[0], "-config-dir", configDir)
	patched = append(patched, args[1:]...)
	return patched
}

func pscalRunEmbedded(args []string) (status int) {
	configDir := pscalPrepareConfigEnv()
	if configDir == "" {
		fmt.Fprintln(os.Stderr, "micro: warning: unable to resolve writable config directory")
	}
	args = pscalInjectConfigDir(args, configDir)
	savedArgs := os.Args
	savedEmbeddedEnv, hadEmbeddedEnv := os.LookupEnv("PSCAL_MICRO_EMBEDDED")
	rt := &pscalRuntimeState{
		embeddedMode: true,
		sessionID:    0,
	}
	pscalPopulateRuntimeLaunchContext(rt)
	pscalRegisterRuntime(rt)
	os.Args = args
	_ = os.Setenv("PSCAL_MICRO_EMBEDDED", "1")
	_, _, _ = pscalSyncGoEnvSizeFromC()
	defer func() {
		os.Args = savedArgs
		pscalUnregisterRuntime(rt)
		if hadEmbeddedEnv {
			_ = os.Setenv("PSCAL_MICRO_EMBEDDED", savedEmbeddedEnv)
		} else {
			_ = os.Unsetenv("PSCAL_MICRO_EMBEDDED")
		}
	}()

	defer func() {
		if r := recover(); r != nil {
			if code, ok := pscalExitCodeFromRecovered(r); ok {
				status = code
				return
			}
			fmt.Fprintf(os.Stderr, "micro: internal panic: %v\n", r)
			fmt.Fprintf(os.Stderr, "micro: panic stack:\n%s\n", debug.Stack())
			status = 1
		}
	}()

	pscalMicroMain(rt)
	return status
}

func pscalArgvToStrings(argc C.int, argv **C.char) []string {
	count := int(argc)
	if count <= 0 {
		return []string{"micro"}
	}
	args := make([]string, 0, count)
	base := uintptr(unsafe.Pointer(argv))
	step := unsafe.Sizeof(*argv)
	for i := 0; i < count; i++ {
		ptr := *(**C.char)(unsafe.Pointer(base + uintptr(i)*step))
		if ptr == nil {
			break
		}
		args = append(args, C.GoString(ptr))
	}
	if len(args) == 0 {
		args = append(args, "micro")
	}
	return args
}

//export pscal_micro_go_main_entry
func pscal_micro_go_main_entry(argc C.int, argv **C.char) C.int {
	args := pscalArgvToStrings(argc, argv)
	status := pscalRunEmbedded(args)
	return C.int(status)
}

//export pscal_micro_go_notify_resize
func pscal_micro_go_notify_resize(sessionID C.uint64_t, cols C.int, rows C.int) C.int {
	if pscalPostRuntimeResize(uint64(sessionID), int(cols), int(rows)) {
		return C.int(1)
	}
	return C.int(0)
}
