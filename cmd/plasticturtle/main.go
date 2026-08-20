// Command pt manages ephemeral Tart VMs that sandbox project directories.
package main

import (
	"os"
)

// version is set at build time via -ldflags.
var version = "dev"

// exitStatus is what pt exits with when Execute returns without an error.
//
// It exists because plasticturtle shell must mirror the remote shell's exit code, and a
// cobra RunE can only report success or an error — a remote exit of 3 is
// neither.
var exitStatus int

func main() {
	// plasticturtle _check-trust is served here, before the cobra tree is built, because
	// the zsh plugin runs it on every directory change with a 10ms budget.
	//
	// Do not mistake this for the reason the budget is met. Measured on an
	// M-series Mac, a Go binary that does nothing but os.Exit(0) already costs
	// ~9ms to start in this environment, and linking the prompt library changes
	// that by less than the measurement noise. Package-level initialization runs
	// for every linked package whatever path main takes, so no arrangement of
	// main can avoid it — skipping the command tree saves only its allocations.
	// The budget is dominated by process startup; see doc/plan.md item 20.
	if len(os.Args) == 3 && os.Args[1] == "_check-trust" {
		os.Exit(checkTrust(os.Args[2]))
	}

	if err := newRootCmd().Execute(); err != nil {
		// Cobra has already printed the error; printing it again here would
		// show the user everything twice. Only the status is ours to set.
		if exitStatus == 0 {
			exitStatus = 1
		}
	}
	os.Exit(exitStatus)
}
