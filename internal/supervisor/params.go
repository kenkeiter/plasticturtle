package supervisor

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kenkeiter/plasticturtle/internal/state"
)

// maxPort is the top of the TCP port space. Port 0 never reaches the
// supervisor: the shell has already resolved every forward to a concrete port
// it proved it could bind.
const maxPort = 65535

// projectIDRe mirrors the shape state.ProjectID produces. state keeps its own
// copy unexported, and duplicating three characters of regexp here is cheaper
// than widening that package's surface — but the two must agree, which the
// instance-name cross-check below enforces at runtime anyway.
var projectIDRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

// validate rejects a parameter set that could not describe a real instance.
//
// Everything checked here is checked because acting on it would be worse than
// failing: a malformed instance name creates a VM that garbage collection is
// forbidden to reclaim, and a project ID that disagrees with the name splits
// the state directory from the VM it is supposed to describe.
func (p *Params) validate() error {
	if p == nil {
		return errors.New("supervisor: nil parameters")
	}
	var errs []error

	if !projectIDRe.MatchString(p.ProjectID) {
		errs = append(errs, fmt.Errorf("supervisor: malformed project id %q", p.ProjectID))
	}
	if !state.InstanceNamePattern.MatchString(p.InstanceName) {
		errs = append(errs, fmt.Errorf("supervisor: malformed instance name %q", p.InstanceName))
	} else if !strings.HasPrefix(p.InstanceName, "pt-"+p.ProjectID+"-") {
		errs = append(errs, fmt.Errorf("supervisor: instance name %q does not belong to project %s", p.InstanceName, p.ProjectID))
	}
	if strings.TrimSpace(p.ConfigHash) == "" {
		errs = append(errs, errors.New("supervisor: missing config hash"))
	}
	if !filepath.IsAbs(p.StateRoot) {
		errs = append(errs, fmt.Errorf("supervisor: state root %q is not absolute", p.StateRoot))
	}

	if p.Config == nil {
		errs = append(errs, errors.New("supervisor: missing config snapshot"))
		return errors.Join(errs...)
	}
	if strings.TrimSpace(p.Config.Image) == "" {
		errs = append(errs, errors.New("supervisor: config names no image"))
	}
	if p.Persist && strings.Contains(p.Config.Image, "/") {
		// A registry reference names a cached OCI image, not a VM anyone owns:
		// booting it in place would write the guest's changes into the image
		// cache, where the next pull silently discards them. pt shell says the
		// same thing on a terminal; this is the layer that catches a _supervise
		// invoked by hand.
		errs = append(errs, fmt.Errorf("supervisor: cannot persist %q: it is a remote image reference, not a local vm", p.Config.Image))
	}
	if !filepath.IsAbs(p.Config.ProjectPath) {
		errs = append(errs, fmt.Errorf("supervisor: project path %q is not absolute", p.Config.ProjectPath))
	}
	for i, m := range p.Config.Mounts {
		if strings.TrimSpace(m.Name) == "" {
			errs = append(errs, fmt.Errorf("supervisor: mounts[%d] has no name", i))
		}
		if !filepath.IsAbs(m.HostPath) {
			errs = append(errs, fmt.Errorf("supervisor: mounts[%d] host path %q is not absolute", i, m.HostPath))
		}
	}

	seen := make(map[int]int, len(p.Ports))
	for i, f := range p.Ports {
		if f.VMPort < 1 || f.VMPort > maxPort {
			errs = append(errs, fmt.Errorf("supervisor: ports[%d] vm port %d out of range", i, f.VMPort))
		}
		if f.HostPort < 1 || f.HostPort > maxPort {
			errs = append(errs, fmt.Errorf("supervisor: ports[%d] host port %d out of range", i, f.HostPort))
			continue
		}
		// Two forwards on one host port would have the second listener fail to
		// bind halfway through setup, which reads as a port conflict with some
		// other program rather than as the caller's bug that it is.
		if first, dup := seen[f.HostPort]; dup {
			errs = append(errs, fmt.Errorf("supervisor: ports[%d] repeats host port %d from ports[%d]", i, f.HostPort, first))
			continue
		}
		seen[f.HostPort] = i
	}
	return errors.Join(errs...)
}
