//go:build pscal_embed && darwin

package main

import "github.com/micro-editor/tcell/v2"

func pscalInstallTcellStdioProvider(rt *pscalRuntimeState) func() {
	if rt == nil || rt.tcellInFD < 0 || rt.tcellOutFD < 0 {
		tcell.SetPSCALStdioProvider(nil)
		return func() {}
	}
	inFD := rt.tcellInFD
	outFD := rt.tcellOutFD
	tcell.SetPSCALStdioProvider(func() (int, int, bool) {
		return inFD, outFD, true
	})
	return func() {
		tcell.SetPSCALStdioProvider(nil)
	}
}
