//go:build pscal_embed

package main

import (
	"testing"

	"github.com/micro-editor/tcell/v2"
)

type resizeAwareTestScreen struct {
	tcell.Screen
	applyCalls int
	lastCols   int
	lastRows   int
	postCalls  int
}

func (s *resizeAwareTestScreen) PSCALApplyResize(cols, rows int) {
	s.applyCalls++
	s.lastCols = cols
	s.lastRows = rows
}

func (s *resizeAwareTestScreen) PostEvent(ev tcell.Event) error {
	s.postCalls++
	return nil
}

type postOnlyTestScreen struct {
	tcell.Screen
	postCalls int
}

func (s *postOnlyTestScreen) PostEvent(ev tcell.Event) error {
	s.postCalls++
	return nil
}

func resetRuntimeRegistryForTest(t *testing.T) {
	t.Helper()
	pscalRuntimeRegistryMu.Lock()
	pscalRuntimeRegistry = map[uint64]*pscalRuntimeState{}
	pscalRuntimeRegistryMu.Unlock()
}

func TestPscalPostRuntimeResizePrefersResizeAwareScreen(t *testing.T) {
	resetRuntimeRegistryForTest(t)
	screen := &resizeAwareTestScreen{}
	rt := &pscalRuntimeState{
		sessionID: 9001,
		screen:    screen,
	}
	pscalRegisterRuntime(rt)
	t.Cleanup(func() {
		pscalUnregisterRuntime(rt)
		resetRuntimeRegistryForTest(t)
	})

	if ok := pscalPostRuntimeResize(9001, 137, 42); !ok {
		t.Fatalf("expected resize dispatch to succeed")
	}
	if screen.applyCalls != 1 {
		t.Fatalf("expected resize-aware apply path once, got %d", screen.applyCalls)
	}
	if screen.postCalls != 0 {
		t.Fatalf("expected PostEvent fallback to stay unused, got %d call(s)", screen.postCalls)
	}
	if screen.lastCols != 137 || screen.lastRows != 42 {
		t.Fatalf("expected applied geometry 137x42, got %dx%d", screen.lastCols, screen.lastRows)
	}
}

func TestPscalPostRuntimeResizeFallsBackToPostEvent(t *testing.T) {
	resetRuntimeRegistryForTest(t)
	screen := &postOnlyTestScreen{}
	rt := &pscalRuntimeState{
		sessionID: 9002,
		screen:    screen,
	}
	pscalRegisterRuntime(rt)
	t.Cleanup(func() {
		pscalUnregisterRuntime(rt)
		resetRuntimeRegistryForTest(t)
	})

	if ok := pscalPostRuntimeResize(9002, 128, 39); !ok {
		t.Fatalf("expected resize dispatch to succeed")
	}
	if screen.postCalls != 1 {
		t.Fatalf("expected PostEvent fallback once, got %d", screen.postCalls)
	}
}
