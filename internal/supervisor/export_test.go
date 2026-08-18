package supervisor

import "testing"

// setSSHPort points the boot path at an in-process sshx.TestServer.
//
// The guest's SSH port is the one thing the frozen Deps cannot vary, and there
// is no SSH seam by design — the tests dial for real. Overriding an unexported
// package variable is the standard way out: it changes no exported API, and it
// buys genuine end-to-end coverage of the dial, the tunnels and the forwarding
// without a hypervisor.
//
// It is process-global, so no test that uses it may run in parallel.
func setSSHPort(t *testing.T, port int) {
	t.Helper()
	prev := sshPort
	sshPort = port
	t.Cleanup(func() { sshPort = prev })
}
