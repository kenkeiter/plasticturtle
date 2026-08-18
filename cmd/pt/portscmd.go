package main

import (
	"context"
	"io"
	"time"

	"github.com/kenkeiter/plasticturtle/internal/config"
	"github.com/kenkeiter/plasticturtle/internal/ports"
	"github.com/kenkeiter/plasticturtle/internal/ptcfg"
	"github.com/kenkeiter/plasticturtle/internal/state"
)

func runPorts(e *env, path string, out io.Writer, global, jsonOut bool) error {
	if global {
		rows, err := ports.GlobalRows(context.Background(), e.Store)
		if err != nil {
			return err
		}
		return ports.Render(out, rows, true, jsonOut)
	}

	p, _, err := loadProject(path)
	if err != nil {
		return err
	}

	// A resolve failure is not fatal here: the configured forwards are still
	// worth showing even if a mount source is missing. Fall back to the
	// unresolved ports so the table is never empty for the wrong reason.
	resolved := p.Resolved
	if resolved == nil {
		resolved = configuredPortsOnly(p)
	}

	id := state.ProjectID(p.Dir)
	inst, ok := readInstanceForStatus(e, id)
	if !ok {
		// No live record: every configured forward is inactive.
		return ports.Render(out, ports.Rows(resolved, nil, false), false, jsonOut)
	}

	healthy := state.Alive(inst.SupervisorPID, inst.SupervisorStart)
	if healthy {
		// A live PID is not enough. A supervisor that has stopped beating may
		// still hold its listeners open or may not; reporting "forwarding" on
		// the strength of a PID alone would be a guess.
		age, err := e.Store.HeartbeatAge(id, time.Now())
		healthy = err == nil && age <= ptcfg.HeartbeatStaleAfter
	}
	return ports.Render(out, ports.Rows(resolved, inst, healthy), false, jsonOut)
}

// configuredPortsOnly builds the minimum config.Resolved that ports.Rows needs
// when full resolution failed — the ports, with host ports defaulted the same
// way Resolve would have defaulted them.
func configuredPortsOnly(p *project) *config.Resolved {
	r := &config.Resolved{ProjectPath: p.Dir, Image: p.Config.Image}
	for _, cp := range p.Config.Ports {
		host := cp.HostPort
		if host == 0 {
			host = cp.VMPort
		}
		r.Ports = append(r.Ports, config.ResolvedPort{VMPort: cp.VMPort, HostPort: host})
	}
	return r
}
