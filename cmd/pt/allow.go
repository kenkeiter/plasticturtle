package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/kenkeiter/plasticturtle/internal/config"
)

// errDeclined reports that the user said no.
//
// It is a sentinel rather than a plain error because declining is a choice, not
// a failure: pt exits non-zero so a script can tell, but printing
// "Error: declined" at somebody who just answered a question is noise.
var errDeclined = errors.New("declined")

// runAllow records trust in a project's config after showing the user exactly
// what that config grants and getting a yes.
//
// This is the security choke point of the whole tool. The summary is not a
// courtesy: a .plasticturtle can mount any host directory read-write into a VM
// that an LLM agent is about to run commands in, and the only thing standing
// between a cloned repo and that outcome is the user reading these lines.
func runAllow(e *env, path string, in io.Reader, out io.Writer) error {
	p, resolveErr, err := loadProject(path)
	if err != nil {
		// Includes validation failure: an invalid config cannot be trusted, and
		// we fail here before prompting rather than asking about something that
		// could never work.
		return err
	}

	writeTrustSummary(out, p, resolveErr)

	fmt.Fprintf(out, "\nAllow it? [y/N]: ")
	answer, _ := bufio.NewReader(in).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
	default:
		// EOF lands here too, which is the behavior we want: a non-interactive
		// `pt allow` declines rather than granting trust nobody confirmed.
		fmt.Fprintln(out, "Not allowed.")
		return errDeclined
	}

	if err := e.Trust.Allow(p.Dir, p.Hash, time.Now()); err != nil {
		return err
	}
	fmt.Fprintf(out, "Allowed %s\n", p.Dir)
	return nil
}

// writeTrustSummary renders everything the config grants: the image it boots,
// the resources it claims, every host directory it exposes and in which mode,
// and every host port it opens.
func writeTrustSummary(out io.Writer, p *project, resolveErr error) {
	fmt.Fprintf(out, "%s in %s\n\n", config.FileName, p.Dir)

	tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
	fmt.Fprintf(tw, "  image\t%s\n", p.Config.Image)

	if r := p.Config.Resources; r != nil && (r.CPU > 0 || r.Memory > 0) {
		fmt.Fprintf(tw, "  resources\t%s\n", describeResources(r))
	} else {
		fmt.Fprintf(tw, "  resources\tinherited from the image\n")
	}
	_ = tw.Flush()

	// Mounts come from the resolved config when available, because an absolute
	// path is what the user is actually granting — "./scratch" tells them less
	// than the directory it names.
	fmt.Fprintf(out, "\n  mounts\n")
	mtw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
	if p.Resolved != nil {
		for _, m := range p.Resolved.Mounts {
			fmt.Fprintf(mtw, "    %s\t%s\t%s\n", m.Name, m.HostPath, describeMode(m.Mode))
		}
	} else {
		// Resolution failed, so show what the file says and let the warning
		// below explain why the paths could not be confirmed.
		fmt.Fprintf(mtw, "    %s\t%s\t%s\n", config.ProjectMountName, p.Dir, describeMode(projectMountMode(p.Config)))
		for _, m := range p.Config.Mounts {
			if m.Name == config.ProjectMountName {
				continue
			}
			fmt.Fprintf(mtw, "    %s\t%s\t%s\n", m.Name, m.HostPath, describeMode(m.Mode))
		}
	}
	_ = mtw.Flush()

	fmt.Fprintf(out, "\n  ports\n")
	ports := p.Config.Ports
	if len(ports) == 0 {
		fmt.Fprintf(out, "    (none)\n")
	}
	ptw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
	for _, pt := range ports {
		host := pt.HostPort
		if host == 0 {
			host = pt.VMPort
		}
		fmt.Fprintf(ptw, "    VM %d\t-> host %d\t(reachable on 127.0.0.1 only)\n", pt.VMPort, host)
	}
	_ = ptw.Flush()

	if resolveErr != nil {
		// Not fatal: a config can be valid and still name a directory the user
		// has not created yet. But pt shell will refuse to boot until it exists,
		// so say so now rather than at the least convenient moment.
		fmt.Fprintf(out, "\n  warning: %v\n", resolveErr)
		fmt.Fprintf(out, "  (pt shell will fail until this is fixed)\n")
	}
}

func describeResources(r *config.Resources) string {
	var parts []string
	if r.CPU > 0 {
		parts = append(parts, fmt.Sprintf("%d vCPU", r.CPU))
	}
	if r.Memory > 0 {
		parts = append(parts, fmt.Sprintf("%d MiB memory", r.Memory))
	}
	return strings.Join(parts, ", ")
}

// describeMode spells out the mode, because "rw" understates what it grants.
func describeMode(m config.Mode) string {
	if m == config.ModeRO {
		return "read-only"
	}
	return "READ-WRITE"
}

func projectMountMode(c *config.Config) config.Mode {
	for _, m := range c.Mounts {
		if m.Name == config.ProjectMountName && m.Mode != "" {
			return m.Mode
		}
	}
	return config.ModeRW
}
