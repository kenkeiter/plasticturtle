package main

import (
	"fmt"
	"os"

	"golang.org/x/term"

	"github.com/kenkeiter/plasticturtle/internal/config"
	"github.com/kenkeiter/plasticturtle/internal/state"
	"github.com/kenkeiter/plasticturtle/internal/sys"
	"github.com/kenkeiter/plasticturtle/internal/tart"
	"github.com/kenkeiter/plasticturtle/internal/trust"
)

// env bundles the collaborators every command needs, so that each RunE opens
// them the same way and tests can substitute a temp state root.
type env struct {
	Store *state.Store
	Trust trust.Store
	Tart  tart.Client
}

// stateRootOverride lets tests point pt at a temp directory. It is not a flag:
// a user pointing pt at an alternate state root would fragment their instance
// records and orphan running VMs.
var stateRootOverride string

func openEnv() (*env, error) {
	root := stateRootOverride
	if root == "" {
		r, err := state.DefaultRoot()
		if err != nil {
			return nil, err
		}
		root = r
	}
	store, err := state.Open(root)
	if err != nil {
		return nil, err
	}
	ts, err := trust.Open(store.TrustPath())
	if err != nil {
		return nil, err
	}
	return &env{
		Store: store,
		Trust: ts,
		Tart:  tart.NewCLI("", sys.RealRunner()),
	}, nil
}

// project is a resolved, validated, loaded project.
type project struct {
	Dir      string
	Config   *config.Config
	Raw      []byte
	Hash     string
	Resolved *config.Resolved
}

// loadProject finds the project at or above path, loads and validates it, and
// resolves its mounts and ports.
//
// resolveErr is returned separately rather than as a failure: a config can be
// entirely valid and still name a mount source that does not exist yet, which
// pt allow wants to warn about rather than refuse.
func loadProject(path string) (p *project, resolveErr error, err error) {
	if path == "" {
		wd, werr := os.Getwd()
		if werr != nil {
			return nil, nil, fmt.Errorf("determine working directory: %w", werr)
		}
		path = wd
	}
	dir, err := config.Find(path)
	if err != nil {
		return nil, nil, err
	}
	cfg, raw, err := config.Load(dir)
	if err != nil {
		return nil, nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, nil, fmt.Errorf("%s is not valid:\n%w", config.FileName, err)
	}
	p = &project{Dir: dir, Config: cfg, Raw: raw, Hash: config.HashBytes(raw)}
	p.Resolved, resolveErr = cfg.Resolve(dir)
	return p, resolveErr, nil
}

// isTerminal reports whether f is an interactive terminal. Commands that must
// prompt refuse rather than hang when it is not.
func isTerminal(f *os.File) bool {
	return f != nil && term.IsTerminal(int(f.Fd()))
}
