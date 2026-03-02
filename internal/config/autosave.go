package config

import (
	"sync"
	"time"
)

var Autosave chan bool
var autotime chan float64
var autosaveMu sync.Mutex
var autosaveStop chan struct{}
var autosaveDone chan struct{}

func init() {
	Autosave = make(chan bool)
	autotime = make(chan float64, 1)
}

func SetAutoTime(a float64) {
	select {
	case autotime <- a:
		return
	default:
	}
	// Replace stale pending values instead of blocking shutdown paths.
	select {
	case <-autotime:
	default:
	}
	select {
	case autotime <- a:
	default:
	}
}

func StartAutoSave() {
	StopAutoSave()

	stop := make(chan struct{})
	done := make(chan struct{})
	autosaveMu.Lock()
	autosaveStop = stop
	autosaveDone = done
	autosaveMu.Unlock()

	go func() {
		defer close(done)
		var a float64
		var t *time.Timer
		var elapsed <-chan time.Time
		for {
			select {
			case <-stop:
				if t != nil {
					t.Stop()
				}
				return
			case a = <-autotime:
				if t != nil {
					t.Stop()
					for len(elapsed) > 0 {
						<-elapsed
					}
				}
				if a > 0 {
					if t != nil {
						t.Reset(time.Duration(a * float64(time.Second)))
					} else {
						t = time.NewTimer(time.Duration(a * float64(time.Second)))
						elapsed = t.C
					}
				}
			case <-elapsed:
				if a > 0 {
					t.Reset(time.Duration(a * float64(time.Second)))
					select {
					case Autosave <- true:
					case <-stop:
						return
					}
				}
			}
		}
	}()
}

func StopAutoSave() {
	autosaveMu.Lock()
	stop := autosaveStop
	done := autosaveDone
	autosaveStop = nil
	autosaveDone = nil
	autosaveMu.Unlock()

	if stop != nil {
		close(stop)
	}
	if done != nil {
		<-done
	}
}
