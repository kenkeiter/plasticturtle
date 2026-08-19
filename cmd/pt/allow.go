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
	"github.com/kenkeiter/plasticturtle/internal/trust"
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
//
// Re-approval shows only what changed since the last one. Re-reading an
// unchanged wall of text is how a reader stops reading, and the whole risk of a
// re-approval lives in the delta — an added mount, a widened mode, a new
// allowed domain — which a full summary buries among lines already approved.
func runAllow(e *env, path string, in io.Reader, out io.Writer) error {
	p, resolveErr, err := loadProject(path)
	if err != nil {
		// Includes validation failure: an invalid config cannot be trusted, and
		// we fail here before prompting rather than asking about something that
		// could never work.
		return err
	}

	// A failure to read the trust database is not fatal here: the worst case is
	// that the user is shown the full summary instead of a diff, and pt allow
	// would rather over-explain than refuse to run.
	prev, hasPrev, _ := e.Trust.Get(p.Dir)

	if hasPrev && prev.Hash == p.Hash {
		// Nothing to approve. Re-prompting for a yes the user already gave,
		// about a file that has not changed by a byte, trains them to answer
		// "y" without reading — the exact habit this prompt depends on not
		// forming.
		fmt.Fprintf(out, "%s in %s\n", config.FileName, p.Dir)
		fmt.Fprintf(out, "Unchanged since you allowed it %s. Nothing to do.\n", humanizeSince(prev.AllowedAt))
		writeResolveWarning(out, resolveErr)
		return nil
	}

	if !writeTrustChanges(out, p, prev, hasPrev) {
		writeTrustSummary(out, p)
	}
	writeResolveWarning(out, resolveErr)

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

	// The raw bytes go into the record so the next pt allow can diff against
	// what was actually approved rather than against the user's memory of it.
	if err := e.Trust.Allow(p.Dir, p.Hash, p.Raw, time.Now()); err != nil {
		return err
	}
	fmt.Fprintf(out, "Allowed %s\n", p.Dir)
	return nil
}

// writeTrustChanges renders the difference between the previously approved
// config and the one on disk. It reports whether it printed anything usable;
// when it cannot — no prior approval, or a record from a pt that did not
// snapshot the file — the caller falls back to the full summary, because
// showing nothing is never the safe default at an approval prompt.
func writeTrustChanges(out io.Writer, p *project, prev trust.Record, hasPrev bool) bool {
	if !hasPrev {
		return false
	}
	if len(prev.Raw) == 0 {
		fmt.Fprintf(out, "%s in %s\n", config.FileName, p.Dir)
		fmt.Fprintf(out, "It changed since you allowed it %s, but that approval predates change tracking, so here is everything it grants:\n\n", humanizeSince(prev.AllowedAt))
		writeTrustGrants(out, grantsOf(p.Config, p.Dir))
		return true
	}
	if config.HashBytes(prev.Raw) != prev.Hash {
		// The store refuses to write a snapshot that disagrees with its hash,
		// so this is a hand-edited or corrupt database. Anyone who can write
		// trust.json can already grant themselves trust outright, but a diff is
		// an argument for saying yes, and it must not be built from bytes that
		// cannot be shown to be the ones approved.
		return false
	}
	prevCfg, err := config.Parse(prev.Raw)
	if err != nil {
		// The snapshot is unreadable by this pt — a schema too old or too new.
		// Fall back rather than guess at what it used to say.
		return false
	}

	changes := diffGrants(grantsOf(prevCfg, p.Dir), grantsOf(p.Config, p.Dir))
	fmt.Fprintf(out, "%s in %s\n", config.FileName, p.Dir)
	if len(changes) == 0 {
		// The bytes changed — that is why trust lapsed — but nothing it grants
		// did. Saying so is the point: it tells the user the edit was comments,
		// formatting, or a field pt does not act on, and that re-approving
		// widens nothing.
		fmt.Fprintf(out, "Edited since you allowed it %s, but nothing it grants has changed.\n", humanizeSince(prev.AllowedAt))
		fmt.Fprintf(out, "(comments, formatting, or ordering only)\n")
		return true
	}
	fmt.Fprintf(out, "Changed since you allowed it %s:\n\n", humanizeSince(prev.AllowedAt))

	tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
	for _, c := range changes {
		var detail string
		switch c.op {
		case '~':
			detail = c.old + " -> " + c.new
			if c.unchanged != "" {
				detail = c.unchanged + "  " + detail
			}
		case '+':
			detail = c.new
		case '-':
			detail = c.old
		}
		if detail == "" {
			// An allowed domain is its own label. Emitting an empty second cell
			// would pad every line out to the longest label for nothing.
			fmt.Fprintf(tw, "  %c %s\n", c.op, c.label)
			continue
		}
		fmt.Fprintf(tw, "  %c %s\t%s\n", c.op, c.label, detail)
	}
	_ = tw.Flush()
	return true
}

// writeTrustSummary renders everything the config grants: the image it boots,
// the resources it claims, every host directory it exposes and in which mode,
// every host port it opens, and how much of the internet the guest can reach.
func writeTrustSummary(out io.Writer, p *project) {
	fmt.Fprintf(out, "%s in %s\n\n", config.FileName, p.Dir)
	writeTrustGrants(out, grantsOf(p.Config, p.Dir))
}

// writeTrustGrants lays the grants out in groups. Mounts and ports get their
// own sections because a list of many is easier to scan than a run of prefixed
// lines; the singletons stay at the top where the reader starts.
func writeTrustGrants(out io.Writer, gs []grant) {
	byKind := map[grantKind][]grant{}
	for _, g := range gs {
		byKind[g.kind] = append(byKind[g.kind], g)
	}

	tw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
	for _, k := range []grantKind{grantImage, grantResources} {
		for _, g := range byKind[k] {
			fmt.Fprintf(tw, "  %s\t%s\n", k, g.value)
		}
	}
	_ = tw.Flush()

	for _, section := range []struct {
		heading string
		kind    grantKind
		// arrow marks a section whose value is a destination rather than a
		// property: a port forward reads as VM 3000 -> host 3000.
		arrow bool
	}{
		{heading: "mounts", kind: grantMount},
		{heading: "ports", kind: grantPort, arrow: true},
	} {
		fmt.Fprintf(out, "\n  %s\n", section.heading)
		items := byKind[section.kind]
		if len(items) == 0 {
			fmt.Fprintf(out, "    (none)\n")
			continue
		}
		stw := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
		for _, g := range items {
			value := g.value
			if section.arrow {
				value = "-> " + value
			}
			fmt.Fprintf(stw, "    %s\t%s\t%s\n", g.name, value, g.note)
		}
		_ = stw.Flush()
	}

	fmt.Fprintf(out, "\n  network\n")
	for _, g := range byKind[grantNetPolicy] {
		fmt.Fprintf(out, "    %s %s\n", g.value, g.note)
	}
	if allow := byKind[grantNetAllow]; len(allow) > 0 {
		for _, g := range allow {
			fmt.Fprintf(out, "      %s\n", g.name)
		}
	} else if isRestricted(byKind[grantNetPolicy]) {
		// An empty allowlist under restricted is legal and severe; it must not
		// look like an omission.
		fmt.Fprintf(out, "      (no domains: all outbound is denied)\n")
	}
}

func isRestricted(policy []grant) bool {
	return len(policy) > 0 && policy[0].value == string(config.NetRestricted)
}

// writeResolveWarning reports a config that is valid but cannot boot.
//
// Not fatal: a config can be valid and still name a directory the user has not
// created yet. But pt shell will refuse to boot until it exists, so say so now
// rather than at the least convenient moment.
func writeResolveWarning(out io.Writer, resolveErr error) {
	if resolveErr == nil {
		return
	}
	fmt.Fprintf(out, "\n  warning: %v\n", resolveErr)
	fmt.Fprintf(out, "  (pt shell will fail until this is fixed)\n")
}

// humanizeSince phrases how long ago an approval happened. The exact timestamp
// is not what the reader needs — "5 minutes ago" versus "3 months ago" is what
// tells them whether they are looking at their own edit or somebody else's.
func humanizeSince(t time.Time) string {
	if t.IsZero() {
		return "at some point"
	}
	d := time.Since(t)
	switch {
	case d < 0:
		// A clock change, or a record written by another machine. Claiming a
		// negative age would be worse than declining to guess.
		return "at some point"
	case d < time.Minute:
		return "moments ago"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute")
	case d < 48*time.Hour:
		return plural(int(d.Hours()), "hour")
	case d < 60*24*time.Hour:
		return plural(int(d.Hours()/24), "day")
	default:
		return plural(int(d.Hours()/24/30), "month")
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit + " ago"
	}
	return fmt.Sprintf("%d %ss ago", n, unit)
}
