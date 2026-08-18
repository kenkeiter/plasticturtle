package ports

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kenkeiter/plasticturtle/internal/config"
	"github.com/kenkeiter/plasticturtle/internal/ptcfg"
	"github.com/kenkeiter/plasticturtle/internal/state"
)

// statusLockWait is how long GlobalRows will wait for a project's shared lock
// before skipping it.
//
// It is deliberately a small multiple of the flock poll interval rather than
// ptcfg.LockTimeout: `pt ports --global` is a status command, and a wedged
// supervisor holding one project's lock must not stall the report on every
// other project for ten seconds apiece.
const statusLockWait = 5 * ptcfg.LockRetryInterval

// Status is a forward's live state, as shown by pt ports.
type Status string

const (
	// StatusForwarding means the instance is running and its supervisor is
	// beating; the listener is up.
	StatusForwarding Status = "forwarding"

	// StatusInactive means the project has no running instance.
	StatusInactive Status = "inactive"

	// StatusStale means an instance record exists but its supervisor has
	// stopped beating — the forward is not to be trusted.
	StatusStale Status = "stale"
)

// Row is one line of the pt ports table.
type Row struct {
	// ProjectPath is set only in --global mode, where rows are grouped by
	// project.
	ProjectPath string

	VMPort           int
	HostPort         int
	OriginalHostPort int
	Status           Status

	// Conflict names another project forwarding the same host port. Only
	// --global can detect this, since it is the only mode that sees every
	// project at once.
	Conflict string
}

// Rows builds the table for a single project from its instance record. A nil
// instance yields inactive rows from the config alone.
//
// With an instance present the rows come from the record, not the config: the
// record is what is actually forwarded, including any remap, and a config that
// has since grown a port is not a forward that exists.
//
// healthy is the caller's supervisor-liveness verdict — a live PID and a
// heartbeat newer than ptcfg.HeartbeatStaleAfter. Only a healthy supervisor
// whose instance has reached state running is forwarding; anything else is
// stale, because during creating and stopping there is no listener up.
func Rows(cfg *config.Resolved, inst *state.Instance, healthy bool) []Row {
	if inst == nil {
		if cfg == nil {
			return nil
		}
		rows := make([]Row, 0, len(cfg.Ports))
		for _, p := range cfg.Ports {
			rows = append(rows, Row{VMPort: p.VMPort, HostPort: p.HostPort, Status: StatusInactive})
		}
		return rows
	}

	st := StatusStale
	if healthy && inst.State == state.StateRunning {
		st = StatusForwarding
	}
	rows := make([]Row, 0, len(inst.Ports))
	for _, p := range inst.Ports {
		rows = append(rows, Row{
			VMPort:           p.VMPort,
			HostPort:         p.HostPort,
			OriginalHostPort: p.OriginalHostPort,
			Status:           st,
		})
	}
	return rows
}

// GlobalRows builds the table across every project with a live supervisor,
// annotating host ports claimed by more than one project.
//
// Projects whose lock is held are skipped rather than waited on; a status
// command must never block behind somebody else's pt shell. The consequence is
// that --global reports what it could see, not necessarily everything.
func GlobalRows(ctx context.Context, s *state.Store) ([]Row, error) {
	if s == nil {
		return nil, errors.New("ports: nil state store")
	}
	ids, err := s.ListProjectIDs()
	if err != nil {
		return nil, err
	}

	// One clock reading for the whole sweep, so two projects beating at the
	// same instant cannot be judged differently by a few milliseconds of walk
	// time. time.Now is used directly because it must be the same clock the
	// filesystem stamps heartbeats with; see state.Heartbeat.
	now := time.Now()

	var rows []Row
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		inst, ok := readInstance(s, id)
		if !ok {
			continue
		}
		// The spec scopes --global to live supervisors: a project whose
		// supervisor is gone has no listeners bound, whatever its record says,
		// and GC is about to reclaim it anyway.
		if !state.Alive(inst.SupervisorPID, inst.SupervisorStart) {
			continue
		}
		healthy := false
		if age, herr := s.HeartbeatAge(id, now); herr == nil && age <= ptcfg.HeartbeatStaleAfter {
			healthy = true
		}
		for _, r := range Rows(nil, inst, healthy) {
			r.ProjectPath = inst.ProjectPath
			rows = append(rows, r)
		}
	}

	sortRows(rows)
	annotateConflicts(rows)
	return rows, nil
}

// readInstance reads one project's record under a short-lived shared lock,
// reporting false for anything it cannot promptly and cleanly read.
//
// TryRLock rather than RLock because RLock waits up to ptcfg.LockTimeout,
// which is the right budget for a command about to mutate and the wrong one
// for a report: a sweep across N wedged projects would cost N×10s. A busy
// project is a skip, not a failure.
func readInstance(s *state.Store, projectID string) (*state.Instance, bool) {
	lk, err := s.TryRLock(projectID, statusLockWait)
	if err != nil {
		// Busy, or gone between listing and locking. Either way this project
		// is somebody else's problem right now.
		return nil, false
	}
	defer func() { _ = lk.Unlock() }()

	inst, err := s.ReadInstance(projectID)
	if err != nil || inst == nil {
		return nil, false
	}
	return inst, true
}

// sortRows gives the table a deterministic order: by project, then by the VM
// port the user wrote in their config, then by host port.
func sortRows(rows []Row) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.ProjectPath != b.ProjectPath {
			return a.ProjectPath < b.ProjectPath
		}
		if a.VMPort != b.VMPort {
			return a.VMPort < b.VMPort
		}
		return a.HostPort < b.HostPort
	})
}

// annotateConflicts fills in Row.Conflict for host ports claimed by more than
// one project.
//
// Two projects can only reach this state by racing: each bound its port when
// nothing else held it, and one of them has since lost the listener (a
// supervisor that died without cleaning up, or a port freed and re-taken).
// Whatever the cause, exactly one of them is actually reachable on that port,
// and the user needs to be told which pair to look at.
func annotateConflicts(rows []Row) {
	claimants := make(map[int]map[string]struct{}, len(rows))
	for _, r := range rows {
		if r.HostPort == 0 || r.ProjectPath == "" {
			continue
		}
		set := claimants[r.HostPort]
		if set == nil {
			set = make(map[string]struct{}, 2)
			claimants[r.HostPort] = set
		}
		set[r.ProjectPath] = struct{}{}
	}
	for i := range rows {
		set := claimants[rows[i].HostPort]
		if len(set) < 2 {
			continue
		}
		others := make([]string, 0, len(set)-1)
		for path := range set {
			if path != rows[i].ProjectPath {
				others = append(others, path)
			}
		}
		sort.Strings(others)
		rows[i].Conflict = strings.Join(others, ", ")
	}
}

// conflictText is the human phrasing of Row.Conflict, shared by the table
// renderer and kept here beside the detection so the two stay in step.
func conflictText(conflict string) string {
	return fmt.Sprintf("conflict: also claimed by %s", conflict)
}
