package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/kenkeiter/plasticturtle/internal/sshx"
	"github.com/kenkeiter/plasticturtle/internal/state"
	"github.com/kenkeiter/plasticturtle/internal/supervisor"
	"github.com/kenkeiter/plasticturtle/internal/sys"
	"github.com/kenkeiter/plasticturtle/internal/tart"
)

// runSupervise is the detached supervisor's entry point.
//
// Its stdout and stderr are already pointed at supervisor.log by the plasticturtle shell
// that spawned it, so writing to out is writing to the log. There is no
// terminal on the other end of anything here: this process must never prompt,
// and its log is the only diagnosis a user gets for a failed boot.
func runSupervise(in io.Reader, out io.Writer) error {
	p, err := supervisor.ParseParams(in)
	if err != nil {
		return err
	}

	// The state root travels in the params rather than being re-derived, so that
	// a supervisor cannot end up managing a different root than the shell that
	// spawned it (a changed HOME or XDG_STATE_HOME between the two would
	// otherwise split them).
	store, err := state.Open(p.StateRoot)
	if err != nil {
		return err
	}

	logf := func(format string, args ...any) {
		fmt.Fprintf(out, "%s %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, args...))
	}
	logf("supervising %s for %s", p.InstanceName, p.Config.ProjectPath)

	deps := supervisor.Deps{
		Tart:  tart.NewCLI("", sys.RealRunner()),
		Store: store,
		Clock: sys.RealClock(),
		Creds: sshx.DefaultCredentials(),
		Logf:  logf,
	}

	// SIGTERM is handled inside supervisor.Run, which needs to answer it during
	// the boot as well as while running, so the context here is plain.
	if err := supervisor.Run(context.Background(), p, deps); err != nil {
		logf("supervisor exiting with error: %v", err)
		return err
	}
	logf("supervisor exited cleanly")
	return nil
}
