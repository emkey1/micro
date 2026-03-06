//go:build !(pscal_embed && darwin)

package main

func pscalInstallTcellStdioProvider(rt *pscalRuntimeState) func() {
	return func() {}
}
