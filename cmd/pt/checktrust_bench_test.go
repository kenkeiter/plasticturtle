package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kenkeiter/plasticturtle/internal/config"
	"github.com/kenkeiter/plasticturtle/internal/ptcfg"
	"github.com/kenkeiter/plasticturtle/internal/zshplugin"
)

// BenchmarkCheckTrust measures the work pt does on every directory change.
//
// ptcfg.CheckTrustBudget documents a 10ms budget "enforced by a benchmark" —
// this is that benchmark. It measures the in-process work only: finding the
// project, hashing the file and reading trust.json. It deliberately does NOT
// measure process startup, which dominates the real command (see doc/plan.md
// item 20) and which no arrangement of this code can reduce.
func BenchmarkCheckTrust(b *testing.B) {
	root := b.TempDir()
	b.Setenv("XDG_STATE_HOME", root)

	prev := stateRootOverride
	stateRootOverride = filepath.Join(root, "plasticturtle")
	b.Cleanup(func() { stateRootOverride = prev })

	e, err := openEnv()
	if err != nil {
		b.Fatal(err)
	}

	project, err := filepath.EvalSymlinks(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	body := []byte("version: 1\nimage: img\n")
	if err := os.WriteFile(filepath.Join(project, config.FileName), body, 0o644); err != nil {
		b.Fatal(err)
	}
	if err := e.Trust.Allow(project, config.HashBytes(body), time.Now()); err != nil {
		b.Fatal(err)
	}

	// A subdirectory, because that is what the zsh hook actually passes: the
	// user's cwd, not the project root.
	sub := filepath.Join(project, "a", "b", "c")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := checkTrust(sub); got != zshplugin.ExitTrusted {
			b.Fatalf("checkTrust = %d, want ExitTrusted", got)
		}
	}
	b.StopTimer()

	if per := time.Duration(b.Elapsed().Nanoseconds() / int64(b.N)); per > ptcfg.CheckTrustBudget {
		b.Errorf("checkTrust took %s per call, over the %s budget", per, ptcfg.CheckTrustBudget)
	}
}
