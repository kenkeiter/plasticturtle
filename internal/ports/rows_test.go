package ports

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/kenkeiter/plasticturtle/internal/config"
	"github.com/kenkeiter/plasticturtle/internal/state"
)

func TestRowsInactiveWithoutInstance(t *testing.T) {
	cfg := &config.Resolved{Ports: []config.ResolvedPort{
		{VMPort: 3000, HostPort: 3000},
		{VMPort: 5432, HostPort: 15432},
	}}
	got := Rows(cfg, nil, false)
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2", len(got))
	}
	for i, r := range got {
		if r.Status != StatusInactive {
			t.Errorf("row[%d].Status = %q, want %q", i, r.Status, StatusInactive)
		}
		if r.ProjectPath != "" {
			t.Errorf("row[%d].ProjectPath = %q, want empty outside --global", i, r.ProjectPath)
		}
	}
	if got[1].HostPort != 15432 || got[1].OriginalHostPort != 0 {
		t.Errorf("row[1] = %+v, want the configured host port and no remap", got[1])
	}
}

func TestRowsWithoutConfigOrInstance(t *testing.T) {
	if got := Rows(nil, nil, true); len(got) != 0 {
		t.Fatalf("got %d rows from nothing, want none", len(got))
	}
}

func TestRowsForwardingWhenHealthy(t *testing.T) {
	inst := &state.Instance{
		State: state.StateRunning,
		Ports: []state.PortMap{{VMPort: 3000, HostPort: 3000}},
	}
	got := Rows(nil, inst, true)
	if len(got) != 1 || got[0].Status != StatusForwarding {
		t.Fatalf("got %+v, want a single forwarding row", got)
	}
}

func TestRowsStaleWhenNotHealthy(t *testing.T) {
	inst := &state.Instance{
		State: state.StateRunning,
		Ports: []state.PortMap{{VMPort: 3000, HostPort: 3000}},
	}
	if got := Rows(nil, inst, false); got[0].Status != StatusStale {
		t.Errorf("Status = %q, want %q for an unhealthy supervisor", got[0].Status, StatusStale)
	}

	// A healthy supervisor mid-boot has no listener bound yet, so "forwarding"
	// would be a lie even though nothing is wrong.
	for _, st := range []state.InstanceState{state.StateCreating, state.StateStopping, state.StateDead} {
		inst.State = st
		if got := Rows(nil, inst, true); got[0].Status != StatusStale {
			t.Errorf("state %q: Status = %q, want %q", st, got[0].Status, StatusStale)
		}
	}
}

func TestRowsCarriesRemapAnnotation(t *testing.T) {
	inst := &state.Instance{
		State: state.StateRunning,
		Ports: []state.PortMap{{VMPort: 5432, HostPort: 15432, OriginalHostPort: 5432}},
	}
	got := Rows(nil, inst, true)
	if got[0].OriginalHostPort != 5432 {
		t.Fatalf("OriginalHostPort = %d, want 5432", got[0].OriginalHostPort)
	}
	if want := "forwarding (remapped from 5432)"; statusText(got[0]) != want {
		t.Errorf("statusText = %q, want %q", statusText(got[0]), want)
	}
}

// writeProject creates a project's state directory, instance record and
// heartbeat. A zero supervisorStart means "record a start time that cannot
// match", i.e. a dead supervisor.
func writeProject(t *testing.T, s *state.Store, projectPath string, live bool, beat time.Duration, ports ...state.PortMap) string {
	t.Helper()
	id := state.ProjectID(projectPath)

	pid, start, err := state.Self()
	if err != nil {
		t.Fatalf("state.Self: %v", err)
	}
	if !live {
		// Same PID, a birth time it never had: exactly the PID-reuse case the
		// liveness check exists for.
		start++
	}

	lk, err := s.Lock(id)
	if err != nil {
		t.Fatalf("lock %s: %v", id, err)
	}
	err = s.WriteInstance(id, &state.Instance{
		InstanceName:    "pt-" + id + "-0000abcd",
		ProjectPath:     projectPath,
		State:           state.StateRunning,
		SupervisorPID:   pid,
		SupervisorStart: start,
		CreatedAt:       time.Now(),
		Ports:           ports,
	})
	if uerr := lk.Unlock(); uerr != nil {
		t.Fatalf("unlock %s: %v", id, uerr)
	}
	if err != nil {
		t.Fatalf("write instance %s: %v", id, err)
	}

	if err := s.Heartbeat(id); err != nil {
		t.Fatalf("heartbeat %s: %v", id, err)
	}
	if beat != 0 {
		// Backdate the beat to simulate a supervisor that has stopped beating
		// but whose process is still around.
		old := time.Now().Add(-beat)
		if err := os.Chtimes(s.HeartbeatPath(id), old, old); err != nil {
			t.Fatalf("backdate heartbeat: %v", err)
		}
	}
	return id
}

func newStore(t *testing.T) *state.Store {
	t.Helper()
	s, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	return s
}

func TestGlobalRowsFlagsCrossProjectConflict(t *testing.T) {
	s := newStore(t)
	writeProject(t, s, "/tmp/proj-a", true, 0, state.PortMap{VMPort: 3000, HostPort: 3000})
	writeProject(t, s, "/tmp/proj-b", true, 0,
		state.PortMap{VMPort: 8080, HostPort: 3000},
		state.PortMap{VMPort: 5432, HostPort: 15432, OriginalHostPort: 5432},
	)

	rows, err := GlobalRows(context.Background(), s)
	if err != nil {
		t.Fatalf("GlobalRows: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3: %+v", len(rows), rows)
	}

	for _, r := range rows {
		if r.Status != StatusForwarding {
			t.Errorf("%s:%d Status = %q, want %q", r.ProjectPath, r.HostPort, r.Status, StatusForwarding)
		}
	}

	var conflicted int
	for _, r := range rows {
		switch {
		case r.HostPort == 3000 && r.ProjectPath == "/tmp/proj-a":
			conflicted++
			if r.Conflict != "/tmp/proj-b" {
				t.Errorf("proj-a Conflict = %q, want /tmp/proj-b", r.Conflict)
			}
		case r.HostPort == 3000 && r.ProjectPath == "/tmp/proj-b":
			conflicted++
			if r.Conflict != "/tmp/proj-a" {
				t.Errorf("proj-b Conflict = %q, want /tmp/proj-a", r.Conflict)
			}
		default:
			if r.Conflict != "" {
				t.Errorf("unshared port %d carries Conflict %q", r.HostPort, r.Conflict)
			}
		}
	}
	if conflicted != 2 {
		t.Errorf("flagged %d rows, want both claimants of host port 3000", conflicted)
	}
}

func TestGlobalRowsExcludesDeadSupervisor(t *testing.T) {
	s := newStore(t)
	writeProject(t, s, "/tmp/proj-live", true, 0, state.PortMap{VMPort: 3000, HostPort: 3000})
	writeProject(t, s, "/tmp/proj-dead", false, 0, state.PortMap{VMPort: 3000, HostPort: 3000})

	rows, err := GlobalRows(context.Background(), s)
	if err != nil {
		t.Fatalf("GlobalRows: %v", err)
	}
	if len(rows) != 1 || rows[0].ProjectPath != "/tmp/proj-live" {
		t.Fatalf("got %+v, want only the live project", rows)
	}
	// With the dead project excluded there is nothing left to collide with.
	if rows[0].Conflict != "" {
		t.Errorf("Conflict = %q, want empty", rows[0].Conflict)
	}
}

func TestGlobalRowsStaleOnMissedHeartbeats(t *testing.T) {
	s := newStore(t)
	writeProject(t, s, "/tmp/proj-wedged", true, time.Minute, state.PortMap{VMPort: 3000, HostPort: 3000})

	rows, err := GlobalRows(context.Background(), s)
	if err != nil {
		t.Fatalf("GlobalRows: %v", err)
	}
	if len(rows) != 1 || rows[0].Status != StatusStale {
		t.Fatalf("got %+v, want a single stale row", rows)
	}
}

func TestGlobalRowsSkipsLockedProject(t *testing.T) {
	s := newStore(t)
	writeProject(t, s, "/tmp/proj-open", true, 0, state.PortMap{VMPort: 3000, HostPort: 3000})
	busy := writeProject(t, s, "/tmp/proj-busy", true, 0, state.PortMap{VMPort: 4000, HostPort: 4000})

	lk, err := s.Lock(busy)
	if err != nil {
		t.Fatalf("lock busy project: %v", err)
	}
	defer func() { _ = lk.Unlock() }()

	// The point is not just the omission but the promptness: a status command
	// must not wait out ptcfg.LockTimeout behind somebody else's plasticturtle shell.
	start := time.Now()
	rows, err := GlobalRows(context.Background(), s)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("GlobalRows: %v", err)
	}
	if elapsed > time.Second {
		t.Errorf("GlobalRows took %s behind a locked project, want it to skip promptly", elapsed)
	}
	if len(rows) != 1 || rows[0].ProjectPath != "/tmp/proj-open" {
		t.Fatalf("got %+v, want only the unlocked project", rows)
	}
}

func TestGlobalRowsEmptyStore(t *testing.T) {
	rows, err := GlobalRows(context.Background(), newStore(t))
	if err != nil {
		t.Fatalf("GlobalRows: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("got %+v, want no rows", rows)
	}
}

func TestGlobalRowsRejectsNilStore(t *testing.T) {
	if _, err := GlobalRows(context.Background(), nil); err == nil {
		t.Fatal("GlobalRows(nil) succeeded")
	}
}
