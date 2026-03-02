package action

import (
	"runtime"
	"sync"
)

var (
	quitMu   sync.RWMutex
	quitFunc func(int)
)

// SetQuitFunc allows the main package to install process shutdown behavior.
func SetQuitFunc(fn func(int)) {
	quitMu.Lock()
	quitFunc = fn
	quitMu.Unlock()
}

// RequestQuit exits the editor through the configured shutdown path.
func RequestQuit(code int) {
	quitMu.RLock()
	fn := quitFunc
	quitMu.RUnlock()
	if fn != nil {
		fn(code)
		return
	}
	// Fallback for tests or unusual call sites that don't install a quit hook.
	runtime.Goexit()
}
