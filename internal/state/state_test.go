package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kenkeiter/plasticturtle/internal/ptcfg"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

// selfSession returns a session record for this process, which is by
// definition alive.
func selfSession(t *testing.T, id string) *Session {
	t.Helper()
	pid, start, err := Self()
	if err != nil {
		t.Fatalf("Self: %v", err)
	}
	return &Session{ID: id, PID: pid, ProcStart: start, StartedAt: time.Now()}
}

func deadSession(id string) *Session {
	return &Session{ID: id, PID: 1 << 30, ProcStart: 424242, StartedAt: time.Now()}
}

func TestDefaultRoot(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/var/xdgstate")
	got, err := DefaultRoot()
	if err != nil {
		t.Fatalf("DefaultRoot: %v", err)
	}
	if want := "/var/xdgstate/plasticturtle"; got != want {
		t.Fatalf("DefaultRoot = %q, want %q", got, want)
	}

	// A relative XDG_STATE_HOME is invalid per the spec and must not be
	// honored: it would follow the caller's cwd.
	t.Setenv("XDG_STATE_HOME", "relative/path")
	got, err = DefaultRoot()
	if err != nil {
		t.Fatalf("DefaultRoot: %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	if want := filepath.Join(home, ".local", "state", "plasticturtle"); got != want {
		t.Fatalf("DefaultRoot = %q, want %q", got, want)
	}

	t.Setenv("XDG_STATE_HOME", "")
	if got, _ = DefaultRoot(); got != filepath.Join(home, ".local", "state", "plasticturtle") {
		t.Fatalf("DefaultRoot with empty XDG_STATE_HOME = %q", got)
	}
}

func TestProjectID(t *testing.T) {
	const path = "/Users/alice/code/myproj"
	id := ProjectID(path)
	if len(id) != 16 {
		t.Fatalf("ProjectID length = %d, want 16", len(id))
	}
	if !projectIDPattern.MatchString(id) {
		t.Fatalf("ProjectID = %q, not lowercase hex", id)
	}
	if again := ProjectID(path); again != id {
		t.Fatalf("ProjectID is not stable: %q then %q", id, again)
	}
	// The value keys state on disk and names VMs; a change here strands every
	// existing project's state, so it is pinned to a literal.
	if want := "9c1e6f81992b0c7c"; id != want {
		t.Fatalf("ProjectID(%q) = %q, want %q", path, id, want)
	}
	if ProjectID(path+"/") == id {
		t.Fatal("ProjectID does not distinguish a trailing separator; callers must canonicalize, but the hash must still differ")
	}
}

func TestNewInstanceNameMatchesPattern(t *testing.T) {
	id := ProjectID("/Users/alice/code/myproj")
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		name, err := NewInstanceName(id)
		if err != nil {
			t.Fatalf("NewInstanceName: %v", err)
		}
		// The generator and the GC safety boundary must agree, always. A name
		// the pattern rejects is a VM GC is forbidden to clean up.
		if !InstanceNamePattern.MatchString(name) {
			t.Fatalf("NewInstanceName = %q, does not match InstanceNamePattern", name)
		}
		if projectIDFromInstanceName(name) != id {
			t.Fatalf("project id not recoverable from %q", name)
		}
		seen[name] = true
	}
	if len(seen) < 900 {
		t.Fatalf("only %d distinct names in 1000 draws; randomness is not random", len(seen))
	}
}

func TestNewInstanceNameRejectsBadProjectID(t *testing.T) {
	for _, id := range []string{"", "short", "1e2a4c3236f1c2e", "1e2a4c3236f1c2ebb", "1E2A4C3236F1C2EB", "../../etc/pass"} {
		if name, err := NewInstanceName(id); err == nil {
			t.Fatalf("NewInstanceName(%q) = %q, want error", id, name)
		}
	}
}

func TestPathHelpers(t *testing.T) {
	s := &Store{Root: "/root"}
	id := "0123456789abcdef"
	cases := map[string]string{
		s.TrustPath():            "/root/trust.json",
		s.ProjectDir(id):         "/root/instances/0123456789abcdef",
		s.InstancePath(id):       "/root/instances/0123456789abcdef/instance.json",
		s.SessionsDir(id):        "/root/instances/0123456789abcdef/sessions",
		s.LockPath(id):           "/root/instances/0123456789abcdef/lock",
		s.HeartbeatPath(id):      "/root/instances/0123456789abcdef/heartbeat",
		s.LogPath(id):            "/root/instances/0123456789abcdef/supervisor.log",
		s.sessionPath(id, "abc"): "/root/instances/0123456789abcdef/sessions/abc.json",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
	}
}

// TestPathHelpersContainTraversal covers the case where a caller's bug supplies
// an ID that is not a single path element. The helpers cannot return an error,
// so they must not produce a path outside the state root.
func TestPathHelpersContainTraversal(t *testing.T) {
	s := &Store{Root: "/root"}
	for _, id := range []string{"../..", "..", ".", "", "a/b", "a\x00b"} {
		dir := s.ProjectDir(id)
		if !strings.HasPrefix(filepath.Clean(dir), "/root/instances/") {
			t.Fatalf("ProjectDir(%q) = %q escapes the state root", id, dir)
		}
	}
}

func TestInstanceRoundTrip(t *testing.T) {
	s := newTestStore(t)
	id := ProjectID("/Users/alice/code/myproj")

	// Absence is not an error; it is the answer "no instance".
	inst, err := s.ReadInstance(id)
	if inst != nil || err != nil {
		t.Fatalf("ReadInstance on empty project = (%v, %v), want (nil, nil)", inst, err)
	}

	name, err := NewInstanceName(id)
	if err != nil {
		t.Fatal(err)
	}
	want := &Instance{
		InstanceName:    name,
		ProjectPath:     "/Users/alice/code/myproj",
		ConfigHash:      "sha256:deadbeef",
		State:           StateRunning,
		SupervisorPID:   4321,
		SupervisorStart: 1787065115672960,
		VMIP:            "192.168.64.5",
		CreatedAt:       time.Now().UTC().Truncate(time.Second),
		Ports: []PortMap{
			{VMPort: 3000, HostPort: 3000},
			{VMPort: 5432, HostPort: 15432, OriginalHostPort: 5432},
		},
	}
	if err := s.WriteInstance(id, want); err != nil {
		t.Fatalf("WriteInstance: %v", err)
	}
	got, err := s.ReadInstance(id)
	if err != nil {
		t.Fatalf("ReadInstance: %v", err)
	}
	if got.InstanceName != want.InstanceName || got.State != want.State ||
		got.SupervisorPID != want.SupervisorPID || got.SupervisorStart != want.SupervisorStart ||
		got.VMIP != want.VMIP || !got.CreatedAt.Equal(want.CreatedAt) || len(got.Ports) != 2 {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, want)
	}
	if !got.Ports[1].Remapped() || got.Ports[0].Remapped() {
		t.Fatalf("Remapped() wrong: %+v", got.Ports)
	}

	if err := s.WriteInstance(id, nil); err == nil {
		t.Fatal("WriteInstance(nil) succeeded")
	}
}

func TestWriteInstanceIsAtomic(t *testing.T) {
	s := newTestStore(t)
	id := ProjectID("/p")

	for i := 0; i < 5; i++ {
		if err := s.WriteInstance(id, &Instance{State: StateRunning, ProjectPath: strings.Repeat("x", 4096)}); err != nil {
			t.Fatalf("WriteInstance: %v", err)
		}
	}

	// No temp file may survive a successful write: a leftover would be
	// indistinguishable from a crashed writer's debris.
	entries, err := os.ReadDir(s.ProjectDir(id))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != "instance.json" {
		t.Fatalf("project dir contains %v, want only instance.json", names)
	}

	// The record on disk is always a complete document, never a prefix.
	b, err := os.ReadFile(s.InstancePath(id))
	if err != nil {
		t.Fatal(err)
	}
	var inst Instance
	if err := json.Unmarshal(b, &inst); err != nil {
		t.Fatalf("instance.json is not valid JSON: %v", err)
	}
}

func TestReadInstanceCorruptRecord(t *testing.T) {
	s := newTestStore(t)
	id := ProjectID("/p")
	if err := os.MkdirAll(s.ProjectDir(id), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.InstancePath(id), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Corruption is not absence: reporting "no instance" here would have pt
	// create a second VM for a project that already has one.
	if _, err := s.ReadInstance(id); err == nil {
		t.Fatal("ReadInstance on corrupt record returned no error")
	}
}

func TestSessionRoundTrip(t *testing.T) {
	s := newTestStore(t)
	id := ProjectID("/p")

	sessions, err := s.ListSessions(id)
	if err != nil || len(sessions) != 0 {
		t.Fatalf("ListSessions on empty project = (%v, %v)", sessions, err)
	}

	a := selfSession(t, "aaaa")
	a.TTY = "/dev/ttys004"
	if err := s.AddSession(id, a); err != nil {
		t.Fatalf("AddSession: %v", err)
	}
	if err := s.AddSession(id, deadSession("bbbb")); err != nil {
		t.Fatalf("AddSession: %v", err)
	}

	sessions, err = s.ListSessions(id)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("ListSessions returned %d sessions, want 2 (stale records included)", len(sessions))
	}
	if sessions[0].TTY != "/dev/ttys004" {
		t.Fatalf("session TTY not round-tripped: %+v", sessions[0])
	}

	// Teardown paths run more than once.
	if err := s.RemoveSession(id, "aaaa"); err != nil {
		t.Fatalf("RemoveSession: %v", err)
	}
	if err := s.RemoveSession(id, "aaaa"); err != nil {
		t.Fatalf("second RemoveSession: %v", err)
	}
}

func TestAddSessionGeneratesID(t *testing.T) {
	s := newTestStore(t)
	id := ProjectID("/p")
	sess := selfSession(t, "")
	if err := s.AddSession(id, sess); err != nil {
		t.Fatalf("AddSession: %v", err)
	}
	if sess.ID == "" {
		t.Fatal("AddSession did not populate the session ID")
	}
	if !sessionIDPattern.MatchString(sess.ID) {
		t.Fatalf("generated session ID %q is not a safe path element", sess.ID)
	}
	got, err := s.ListSessions(id)
	if err != nil || len(got) != 1 || got[0].ID != sess.ID {
		t.Fatalf("ListSessions = (%v, %v)", got, err)
	}
}

func TestSessionIDValidation(t *testing.T) {
	s := newTestStore(t)
	id := ProjectID("/p")
	// An empty ID is not invalid — AddSession assigns one, because the ID is a
	// storage concern rather than something the caller should have to invent.
	for _, bad := range []string{"../instance", "a/b", ".hidden", strings.Repeat("x", 65)} {
		if err := s.AddSession(id, &Session{ID: bad, PID: 1}); err == nil {
			t.Errorf("AddSession(%q) succeeded", bad)
		}
		if err := s.RemoveSession(id, bad); err == nil {
			t.Errorf("RemoveSession(%q) succeeded", bad)
		}
	}
	if err := s.RemoveSession(id, ""); err == nil {
		t.Error("RemoveSession(\"\") succeeded")
	}
	if err := s.AddSession(id, nil); err == nil {
		t.Error("AddSession(nil) succeeded")
	}
}

func TestLiveSessionsPrunesDeadRecords(t *testing.T) {
	s := newTestStore(t)
	id := ProjectID("/p")

	if err := s.AddSession(id, selfSession(t, "alive")); err != nil {
		t.Fatal(err)
	}
	if err := s.AddSession(id, deadSession("dead")); err != nil {
		t.Fatal(err)
	}
	// A PID that exists but was born at a different time is a reused PID, not
	// a live session.
	pid, start, err := Self()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddSession(id, &Session{ID: "reused", PID: pid, ProcStart: start + 1, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	// An unreadable record can never be proven live and would pin the VM open.
	if err := os.WriteFile(filepath.Join(s.SessionsDir(id), "garbage.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	live, err := s.LiveSessions(id)
	if err != nil {
		t.Fatalf("LiveSessions: %v", err)
	}
	if len(live) != 1 || live[0].ID != "alive" {
		t.Fatalf("LiveSessions = %v, want only the live one", live)
	}

	remaining, err := s.ListSessions(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].ID != "alive" {
		t.Fatalf("dead records survived on disk: %v", remaining)
	}
	entries, err := os.ReadDir(s.SessionsDir(id))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("sessions dir holds %d files, want 1", len(entries))
	}
}

func TestLiveSessionsOnMissingDir(t *testing.T) {
	s := newTestStore(t)
	live, err := s.LiveSessions(ProjectID("/never-used"))
	if err != nil || len(live) != 0 {
		t.Fatalf("LiveSessions = (%v, %v), want (empty, nil)", live, err)
	}
}

func TestHeartbeat(t *testing.T) {
	s := newTestStore(t)
	id := ProjectID("/p")

	if _, err := s.HeartbeatAge(id, time.Now()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("HeartbeatAge before any beat = %v, want ErrNotExist", err)
	}
	if err := s.Heartbeat(id); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	age, err := s.HeartbeatAge(id, time.Now())
	if err != nil {
		t.Fatalf("HeartbeatAge: %v", err)
	}
	if age > time.Second {
		t.Fatalf("fresh heartbeat reported age %v", age)
	}

	fi, err := os.Stat(s.HeartbeatPath(id))
	if err != nil {
		t.Fatal(err)
	}
	age, err = s.HeartbeatAge(id, fi.ModTime().Add(ptcfg.HeartbeatStaleAfter+time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if age <= ptcfg.HeartbeatStaleAfter {
		t.Fatalf("HeartbeatAge = %v, want more than %v", age, ptcfg.HeartbeatStaleAfter)
	}

	// A beat stamped in the future (clock adjustment) must not read as
	// negative age, which would compare as "fresh" in one direction only.
	if age, err = s.HeartbeatAge(id, fi.ModTime().Add(-time.Hour)); err != nil || age != 0 {
		t.Fatalf("HeartbeatAge with a past clock = (%v, %v), want (0, nil)", age, err)
	}

	// Beating again must advance the mtime.
	before := fi.ModTime()
	time.Sleep(10 * time.Millisecond)
	if err := s.Heartbeat(id); err != nil {
		t.Fatal(err)
	}
	fi2, err := os.Stat(s.HeartbeatPath(id))
	if err != nil {
		t.Fatal(err)
	}
	if !fi2.ModTime().After(before) {
		t.Fatalf("heartbeat mtime did not advance: %v then %v", before, fi2.ModTime())
	}
}

func TestListProjectIDs(t *testing.T) {
	s := newTestStore(t)
	ids := []string{ProjectID("/a"), ProjectID("/b")}
	for _, id := range ids {
		if err := s.WriteInstance(id, &Instance{State: StateRunning}); err != nil {
			t.Fatal(err)
		}
	}
	// Litter that could not have come from ProjectID is not ours to report,
	// because GC acts on this list.
	for _, junk := range []string{"not-a-project", "ZZZZ", "0123456789abcdefg"} {
		if err := os.MkdirAll(filepath.Join(s.instancesDir(), junk), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(s.instancesDir(), "0000000000000000"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListProjectIDs()
	if err != nil {
		t.Fatalf("ListProjectIDs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListProjectIDs = %v, want the 2 real projects", got)
	}
}

func TestListProjectIDsOnFreshStore(t *testing.T) {
	s := &Store{Root: filepath.Join(t.TempDir(), "nonexistent")}
	got, err := s.ListProjectIDs()
	if err != nil || len(got) != 0 {
		t.Fatalf("ListProjectIDs = (%v, %v), want (empty, nil)", got, err)
	}
}

func TestRemoveProject(t *testing.T) {
	s := newTestStore(t)
	id := ProjectID("/p")
	if err := s.WriteInstance(id, &Instance{State: StateRunning}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddSession(id, selfSession(t, "sess")); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveProject(id); err != nil {
		t.Fatalf("RemoveProject: %v", err)
	}
	if _, err := os.Stat(s.ProjectDir(id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("project dir survived RemoveProject: %v", err)
	}
	// Idempotent: crash-recovery paths run it more than once.
	if err := s.RemoveProject(id); err != nil {
		t.Fatalf("second RemoveProject: %v", err)
	}
}

func TestLockSerializesWriters(t *testing.T) {
	s := newTestStore(t)
	id := ProjectID("/p")

	var held atomic.Bool
	var overlaps atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				lk, err := s.Lock(id)
				if err != nil {
					t.Errorf("Lock: %v", err)
					return
				}
				if !held.CompareAndSwap(false, true) {
					overlaps.Add(1)
				}
				time.Sleep(time.Millisecond)
				held.Store(false)
				if err := lk.Unlock(); err != nil {
					t.Errorf("Unlock: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if n := overlaps.Load(); n != 0 {
		t.Fatalf("%d overlapping exclusive lock holders", n)
	}
}

func TestRLockAllowsConcurrentReaders(t *testing.T) {
	s := newTestStore(t)
	id := ProjectID("/p")

	const readers = 4
	entered := make(chan struct{}, readers)
	release := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lk, err := s.RLock(id)
			if err != nil {
				t.Errorf("RLock: %v", err)
				entered <- struct{}{}
				return
			}
			entered <- struct{}{}
			<-release
			if err := lk.Unlock(); err != nil {
				t.Errorf("Unlock: %v", err)
			}
		}()
	}
	// If shared locks did not overlap, this would deadlock: no reader can
	// release until every reader is inside.
	for i := 0; i < readers; i++ {
		select {
		case <-entered:
		case <-time.After(ptcfg.LockTimeout):
			t.Fatal("shared locks did not overlap")
		}
	}
	close(release)
	wg.Wait()
}

func TestExclusiveLockBlocksAndTimesOut(t *testing.T) {
	s := newTestStore(t)
	id := ProjectID("/p")

	lk, err := s.Lock(id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lk.Unlock() }()

	// The short-deadline path GC uses must give up rather than wait.
	start := time.Now()
	if _, err := s.acquire(id, true, gcLockWait); !errors.Is(err, errLockBusy) {
		t.Fatalf("acquire on a held lock = %v, want errLockBusy", err)
	}
	if elapsed := time.Since(start); elapsed > ptcfg.LockTimeout/2 {
		t.Fatalf("acquire waited %v before giving up", elapsed)
	}
	if _, err := s.acquire(id, false, gcLockWait); !errors.Is(err, errLockBusy) {
		t.Fatalf("shared acquire on a held exclusive lock = %v, want errLockBusy", err)
	}
}

func TestUnlockIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	id := ProjectID("/p")
	lk, err := s.Lock(id)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := lk.Unlock(); err != nil {
			t.Fatalf("Unlock #%d: %v", i+1, err)
		}
	}
	// A second Unlock must not have released a lock somebody else now holds.
	other, err := s.Lock(id)
	if err != nil {
		t.Fatalf("Lock after repeated Unlock: %v", err)
	}
	if err := lk.Unlock(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.acquire(id, true, gcLockWait); !errors.Is(err, errLockBusy) {
		t.Fatal("a repeated Unlock released another holder's lock")
	}
	if err := other.Unlock(); err != nil {
		t.Fatal(err)
	}

	var nilLock *Lock
	if err := nilLock.Unlock(); err != nil {
		t.Fatalf("nil Lock.Unlock: %v", err)
	}
}

// TestConcurrentSessionChurn is the shape of N `pt shell` processes attaching
// to and leaving one instance at once.
func TestConcurrentSessionChurn(t *testing.T) {
	s := newTestStore(t)
	id := ProjectID("/p")
	if err := s.WriteInstance(id, &Instance{State: StateRunning}); err != nil {
		t.Fatal(err)
	}

	const workers = 24
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sessID := fmt.Sprintf("sess-%02d", i)

			lk, err := s.Lock(id)
			if err != nil {
				t.Errorf("Lock: %v", err)
				return
			}
			addErr := s.AddSession(id, selfSession(t, sessID))
			_ = lk.Unlock()
			if addErr != nil {
				t.Errorf("AddSession: %v", addErr)
				return
			}

			// A reader racing the writers must never see a broken directory.
			rl, err := s.RLock(id)
			if err != nil {
				t.Errorf("RLock: %v", err)
				return
			}
			_, listErr := s.ListSessions(id)
			_ = rl.Unlock()
			if listErr != nil {
				t.Errorf("ListSessions: %v", listErr)
				return
			}

			if i%2 == 0 {
				lk, err := s.Lock(id)
				if err != nil {
					t.Errorf("Lock: %v", err)
					return
				}
				rmErr := s.RemoveSession(id, sessID)
				_ = lk.Unlock()
				if rmErr != nil {
					t.Errorf("RemoveSession: %v", rmErr)
				}
			}
		}(i)
	}
	wg.Wait()

	lk, err := s.Lock(id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lk.Unlock() }()

	sessions, err := s.ListSessions(id)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != workers/2 {
		t.Fatalf("%d sessions survived, want %d", len(sessions), workers/2)
	}
	for _, sess := range sessions {
		if sess.PID != os.Getpid() || sess.ProcStart == 0 {
			t.Fatalf("session record damaged: %+v", sess)
		}
	}
	// Nothing but the surviving records may be left behind — no temp files.
	entries, err := os.ReadDir(s.SessionsDir(id))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != workers/2 {
		t.Fatalf("sessions dir holds %d files, want %d", len(entries), workers/2)
	}

	live, err := s.LiveSessions(id)
	if err != nil {
		t.Fatalf("LiveSessions: %v", err)
	}
	if len(live) != workers/2 {
		t.Fatalf("LiveSessions = %d, want %d", len(live), workers/2)
	}
}
