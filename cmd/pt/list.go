package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/kenkeiter/plasticturtle/internal/ptcfg"
	"github.com/kenkeiter/plasticturtle/internal/state"
)

// listRow is one instance as pt list reports it. The json tags are a
// documented interface; renaming one is a breaking change for anything parsing
// --json output.
type listRow struct {
	Project string `json:"project"`
	VM      string `json:"vm"`
	State   string `json:"state"`

	// Persist distinguishes a VM pt will destroy from the user's own image,
	// booted in place by pt shell --persist and left behind afterwards.
	Persist       bool    `json:"persist"`
	Sessions      int     `json:"sessions"`
	CPUPercent    float64 `json:"cpuPercent"`
	MemBytes      uint64  `json:"memBytes"`
	DiskBytes     uint64  `json:"diskBytes"`
	UptimeSeconds float64 `json:"uptimeSeconds"`
}

func runList(e *env, out io.Writer, jsonOut bool) error {
	ctx := context.Background()

	// Opportunistic GC: a status command is exactly where a user notices stale
	// rows, so clean up before reporting rather than reporting garbage.
	if err := e.Store.GC(ctx, e.Tart); err != nil {
		// Not fatal. GC failing should not prevent the user from seeing what is
		// running — that is the information they asked for.
		//
		// It goes to stderr, not out: under --json, a warning on stdout would
		// prepend a bare line to the array and break every parser downstream.
		fmt.Fprintf(os.Stderr, "warning: garbage collection failed: %v\n", err)
	}

	rows, err := collectListRows(e)
	if err != nil {
		return err
	}

	if jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if rows == nil {
			rows = []listRow{}
		}
		return enc.Encode(rows)
	}

	if len(rows) == 0 {
		fmt.Fprintln(out, "No active instances.")
		return nil
	}

	tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "PROJECT\tVM\tMODE\tSTATE\tSESSIONS\tCPU %\tMEM\tDISK*\tUPTIME")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%.1f\t%s\t%s\t%s\n",
			r.Project, r.VM, vmMode(r.Persist), r.State, r.Sessions,
			r.CPUPercent, humanBytes(r.MemBytes), humanBytes(r.DiskBytes),
			humanDuration(time.Duration(r.UptimeSeconds)*time.Second))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	// Stated rather than implied: a CoW clone shares blocks with its source
	// image, and du charges shared blocks to whichever path it walks first.
	fmt.Fprintln(out, "\n* approximate: CoW clones share blocks with the source image.")
	return nil
}

// vmMode names what happens to a VM when its last shell exits, which is the
// distinction the rest of the row cannot show.
func vmMode(persist bool) string {
	if persist {
		return "persist"
	}
	return "clone"
}

func collectListRows(e *env) ([]listRow, error) {
	ids, err := e.Store.ListProjectIDs()
	if err != nil {
		return nil, err
	}
	now := time.Now()

	var rows []listRow
	for _, id := range ids {
		inst, ok := readInstanceForStatus(e, id)
		if !ok {
			continue
		}

		// The VM column names what tart is running, which for a --persist
		// instance is the project's base image rather than a clone. Reporting
		// the instance name there would name something that does not exist.
		r := listRow{
			Project: inst.ProjectPath,
			VM:      inst.VM(),
			State:   string(inst.State),
			Persist: inst.Persist,
		}
		// The recorded state is only a claim; the supervisor's liveness is what
		// makes it true. A record saying "running" whose supervisor is gone is
		// reported dead, because that is what it is.
		if !state.Alive(inst.SupervisorPID, inst.SupervisorStart) {
			r.State = string(state.StateDead)
		}
		r.Sessions = liveSessionCount(e, id)
		if stats, err := state.TreeStats(inst.SupervisorPID); err == nil {
			r.CPUPercent = stats.CPUPercent
			r.MemBytes = stats.RSSBytes
		}
		if dir, err := state.TartVMDir(inst.VM()); err == nil {
			if n, err := state.DiskUsageBytes(dir); err == nil {
				r.DiskBytes = n
			}
		}
		if !inst.CreatedAt.IsZero() {
			r.UptimeSeconds = now.Sub(inst.CreatedAt).Seconds()
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Project < rows[j].Project })
	return rows, nil
}

// readInstanceForStatus reads a record under a short shared lock, reporting
// false for anything it cannot promptly and cleanly read.
func readInstanceForStatus(e *env, projectID string) (*state.Instance, bool) {
	lk, err := e.Store.TryRLock(projectID, ptcfg.StatusLockWait)
	if err != nil {
		return nil, false
	}
	defer func() { _ = lk.Unlock() }()

	inst, err := e.Store.ReadInstance(projectID)
	if err != nil || inst == nil {
		return nil, false
	}
	return inst, true
}

// liveSessionCount counts sessions whose process still exists.
//
// It filters here rather than calling LiveSessions, which prunes dead records
// and therefore needs the exclusive lock. A status command should not have to
// take a write lock to count something.
func liveSessionCount(e *env, projectID string) int {
	sessions, err := e.Store.ListSessions(projectID)
	if err != nil {
		return 0
	}
	n := 0
	for _, s := range sessions {
		if state.Alive(s.PID, s.ProcStart) {
			n++
		}
	}
	return n
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	value := float64(n)
	for _, suffix := range []string{"K", "M", "G", "T"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f%s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1fP", value/unit)
}

func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
}
