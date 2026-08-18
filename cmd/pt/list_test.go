package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kenkeiter/plasticturtle/internal/state"
)

// withInstance writes a running instance record for a project, so pt list has
// something to render.
//
// The supervisor PID is this test process: state.Alive requires both a live PID
// and a matching start time, so anything fabricated would be reported dead and
// the STATE column would never exercise the running path.
func withInstance(t *testing.T, e *env, projectPath string, createdAt time.Time) string {
	t.Helper()

	pid, procStart, err := state.Self()
	if err != nil {
		t.Fatal(err)
	}
	id := state.ProjectID(projectPath)
	name, err := state.NewInstanceName(id)
	if err != nil {
		t.Fatal(err)
	}

	lk, err := e.Store.Lock(id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lk.Unlock() }()

	if err := e.Store.WriteInstance(id, &state.Instance{
		InstanceName:    name,
		ProjectPath:     projectPath,
		ConfigHash:      "sha256:whatever",
		State:           state.StateRunning,
		SupervisorPID:   pid,
		SupervisorStart: procStart,
		VMIP:            "192.168.64.7",
		CreatedAt:       createdAt,
		Ports:           []state.PortMap{{VMPort: 3000, HostPort: 13000, OriginalHostPort: 3000}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.AddSession(id, &state.Session{PID: pid, ProcStart: procStart, StartedAt: createdAt}); err != nil {
		t.Fatal(err)
	}
	return name
}

// The table renders every column spec section 4.5 lists, plus the DISK*
// footnote plan item 6 requires.
//
// The previous test for this asserted only that an EMPTY store printed "No
// active instances" — with zero rows the header and footnote are never emitted,
// so it could not fail on the thing its name promised.
func TestListTableRendersEveryColumn(t *testing.T) {
	e := testEnv(t)
	project := newProject(t, sampleConfig)
	name := withInstance(t, e, project, time.Now().Add(-90*time.Minute))

	var out bytes.Buffer
	if err := runList(e, &out, false); err != nil {
		t.Fatalf("runList: %v", err)
	}
	got := out.String()

	for _, want := range []string{
		"PROJECT", "VM", "STATE", "SESSIONS", "CPU %", "MEM",
		"DISK*", // plan item 6: the asterisk is the whole point
		"UPTIME",
		project, // the project path
		name,    // the instance name
		"running",
		"1h30m", // uptime, from CreatedAt
		"* approximate: CoW clones share blocks with the source image.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("pt list output is missing %q:\n%s", want, got)
		}
	}
}

// A record claiming to be running whose supervisor is gone must be reported
// dead. The record is only a claim; process liveness is what makes it true.
//
// The project's lock is held for the duration, which is what makes this test
// about the STATE override at all: unheld, pt list's opportunistic GC reclaims
// the record before it can be rendered — the correct outcome, and the reason
// this override only matters for a project GC had to skip.
func TestListReportsDeadWhenSupervisorIsGone(t *testing.T) {
	e := testEnv(t)
	project := newProject(t, sampleConfig)
	id := state.ProjectID(project)

	name, err := state.NewInstanceName(id)
	if err != nil {
		t.Fatal(err)
	}
	lk, err := e.Store.Lock(id)
	if err != nil {
		t.Fatal(err)
	}
	// PID 1 is alive but its start time cannot match the fabricated one, which
	// is exactly the PID-reuse case the liveness check exists for.
	if err := e.Store.WriteInstance(id, &state.Instance{
		InstanceName:    name,
		ProjectPath:     project,
		State:           state.StateRunning,
		SupervisorPID:   1,
		SupervisorStart: 12345,
		CreatedAt:       time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := lk.Unlock(); err != nil {
		t.Fatal(err)
	}

	// Shared lock: GC needs the exclusive one and will skip this project, while
	// the status read (also shared) still succeeds.
	held, err := e.Store.RLock(id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Unlock() }()

	var out bytes.Buffer
	if err := runList(e, &out, false); err != nil {
		t.Fatalf("runList: %v", err)
	}
	if !strings.Contains(out.String(), string(state.StateDead)) {
		t.Errorf("a running record with a dead supervisor was not reported dead:\n%s", out.String())
	}
}

// The --json shape is a documented interface; every key must be present.
func TestListJSONCarriesEveryField(t *testing.T) {
	e := testEnv(t)
	project := newProject(t, sampleConfig)
	name := withInstance(t, e, project, time.Now().Add(-2*time.Minute))

	var out bytes.Buffer
	if err := runList(e, &out, true); err != nil {
		t.Fatalf("runList: %v", err)
	}

	var rows []map[string]any
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("--json is not valid JSON: %v\n%s", err, out.String())
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1:\n%s", len(rows), out.String())
	}
	for _, key := range []string{
		"project", "vm", "state", "sessions",
		"cpuPercent", "memBytes", "diskBytes", "uptimeSeconds",
	} {
		if _, ok := rows[0][key]; !ok {
			t.Errorf("--json row is missing key %q:\n%s", key, out.String())
		}
	}
	if rows[0]["vm"] != name {
		t.Errorf("vm = %v, want %s", rows[0]["vm"], name)
	}
	if rows[0]["sessions"].(float64) != 1 {
		t.Errorf("sessions = %v, want 1", rows[0]["sessions"])
	}
}

// A GC warning must never reach stdout under --json: a bare line before the
// array breaks every parser downstream.
func TestListJSONStaysParseableWhenGCWarns(t *testing.T) {
	e := testEnv(t)
	project := newProject(t, sampleConfig)
	withInstance(t, e, project, time.Now())

	// Point the store at a root that cannot be listed, so GC fails.
	if err := os.RemoveAll(e.Store.Root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.Store.Root, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runList(e, &out, true); err != nil {
		// A hard failure is acceptable here; emitting invalid JSON is not.
		return
	}
	var rows []listRow
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Errorf("--json output was not parseable after a GC warning: %v\n%s", err, out.String())
	}
}

func TestHumanDuration(t *testing.T) {
	tests := []struct {
		in   time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{90 * time.Second, "1m30s"},
		{90 * time.Minute, "1h30m"},
		{50 * time.Hour, "2d2h"},
	}
	for _, tc := range tests {
		if got := humanDuration(tc.in); got != tc.want {
			t.Errorf("humanDuration(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
