// Command pt manages ephemeral Tart VMs that sandbox project directories.
package main

import (
	"fmt"
	"os"
)

// version is set at build time via -ldflags.
var version = "dev"

func main() {
	if err := newRootCmd().Execute(); err != nil {
		// Cobra has already printed the error for most failures; exit codes
		// carry the meaning. Anything that needs a specific code (a remote
		// shell's status, _check-trust's 10) exits directly from its RunE.
		fmt.Fprintln(os.Stderr, "pt:", err)
		os.Exit(1)
	}
}
