package state

import (
	"errors"
	"io/fs"
	"os"
	"testing"
	"time"

	"github.com/kenkeiter/plasticturtle/internal/ptcfg"
)

// TryRLock exists so that status sweeps and pollers behave differently from
// interactive commands: they must not wait out ptcfg.LockTimeout, and they must
// not bring state into existence just by looking at it.

func TestTryRLockDoesNotCreateProjectDir(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := ProjectID("/nonexistent/project")

	if _, err := s.TryRLock(id, statusWait); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("TryRLock on an absent project = %v, want fs.ErrNotExist", err)
	}
	// The point of the no-create path: a poll that races teardown must not
	// resurrect the directory the supervisor just removed.
	if _, err := os.Stat(s.ProjectDir(id)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("TryRLock created %s; it must only ever observe", s.ProjectDir(id))
	}
}

func TestRLockStillCreatesProjectDir(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := ProjectID("/some/project")

	lk, err := s.RLock(id)
	if err != nil {
		t.Fatalf("RLock: %v", err)
	}
	defer func() { _ = lk.Unlock() }()

	if _, err := os.Stat(s.ProjectDir(id)); err != nil {
		t.Errorf("RLock did not create the project dir: %v", err)
	}
}

func TestTryRLockGivesUpQuicklyWhenBusy(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := ProjectID("/busy/project")

	held, err := s.Lock(id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Unlock() }()

	start := time.Now()
	_, err = s.TryRLock(id, statusWait)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrLockBusy) {
		t.Fatalf("TryRLock on a locked project = %v, want ErrLockBusy", err)
	}
	// The whole reason this method exists: a sweep across N wedged projects
	// must not cost N x LockTimeout.
	if elapsed >= ptcfg.LockTimeout {
		t.Errorf("waited %s, i.e. the full LockTimeout; the short deadline was ignored", elapsed)
	}
}

func TestTryRLockAllowsConcurrentReaders(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := ProjectID("/shared/project")

	// Bring the directory into existence the way a real create path would.
	seed, err := s.Lock(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Unlock(); err != nil {
		t.Fatal(err)
	}

	first, err := s.TryRLock(id, statusWait)
	if err != nil {
		t.Fatalf("first TryRLock: %v", err)
	}
	defer func() { _ = first.Unlock() }()

	second, err := s.TryRLock(id, statusWait)
	if err != nil {
		t.Fatalf("second TryRLock while the first is held: %v", err)
	}
	if err := second.Unlock(); err != nil {
		t.Errorf("Unlock: %v", err)
	}
}

// statusWait mirrors the deadline status callers pass; small multiples of the
// poll interval, not the interactive timeout.
const statusWait = 5 * ptcfg.LockRetryInterval
