//go:build pscal_embed

package main

/*
#include <stdint.h>
*/
import "C"

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"unsafe"
)

var pscalEmbedRunState struct {
	sync.Mutex
	running bool
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
	pscalEmbedRunState.Lock()
	if pscalEmbedRunState.running {
		pscalEmbedRunState.Unlock()
		fmt.Fprintln(os.Stderr, "micro: already running")
		return 1
	}
	pscalEmbedRunState.running = true
	pscalEmbedRunState.Unlock()
	defer func() {
		pscalEmbedRunState.Lock()
		pscalEmbedRunState.running = false
		pscalEmbedRunState.Unlock()
	}()

	configDir := pscalPrepareConfigEnv()
	if configDir == "" {
		fmt.Fprintln(os.Stderr, "micro: warning: unable to resolve writable config directory")
	}
	args = pscalInjectConfigDir(args, configDir)
	savedArgs := os.Args
	savedEmbeddedEnv, hadEmbeddedEnv := os.LookupEnv("PSCAL_MICRO_EMBEDDED")
	os.Args = args
	pscalEmbeddedMode = true
	_ = os.Setenv("PSCAL_MICRO_EMBEDDED", "1")
	defer func() {
		pscalEmbeddedMode = false
		os.Args = savedArgs
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

	pscalMicroMain()
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
