package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kenkeiter/plasticturtle/internal/config"
)

// rewriteConfig replaces a project's config, simulating the edit that costs it
// its trust.
func rewriteConfig(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, config.FileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// allowOnce approves a project and returns what the user was shown.
func allowOnce(t *testing.T, e *env, dir string) string {
	t.Helper()
	var out bytes.Buffer
	if err := runAllow(e, dir, strings.NewReader("y\n"), &out); err != nil {
		t.Fatalf("runAllow: %v\n%s", err, out.String())
	}
	return out.String()
}

func TestAllowShowsOnlyWhatChanged(t *testing.T) {
	e := testEnv(t)
	keep, add := t.TempDir(), t.TempDir()
	project := newProject(t, "version: 1\nimage: img:1\n"+
		"ports:\n  - vm_port: 3000\n  - vm_port: 5432\n    host_port: 15432\n"+
		"mounts:\n  - name: keep\n    host_path: "+keep+"\n    mode: ro\n"+
		"network:\n  policy: restricted\n  allow:\n    - github.com\n    - old.example.com\n")
	allowOnce(t, e, project)

	// Every kind of change at once: image bumped, a mount widened to
	// read-write, a mount added, a port dropped, a port remapped, one domain
	// swapped for another. The "keep" mount and the github.com rule are
	// untouched, and are what must NOT reappear.
	rewriteConfig(t, project, "version: 1\nimage: img:2\n"+
		"ports:\n  - vm_port: 5432\n    host_port: 25432\n"+
		"mounts:\n  - name: keep\n    host_path: "+keep+"\n    mode: rw\n"+
		"  - name: added\n    host_path: "+add+"\n"+
		"network:\n  policy: restricted\n  allow:\n    - github.com\n    - new.example.com\n")

	got := allowOnce(t, e, project)

	for _, want := range []string{
		"~ image",           // img:1 -> img:2
		"img:1 -> img:2",    //
		"~ mount keep",      // the mode widened; that is the dangerous one
		"read-only -> READ", //
		"+ mount added",     //
		add,                 // and where the new mount points
		"- port VM 3000",    // a forward withdrawn
		"~ port VM 5432",    // and one remapped
		"host 15432 -> host 25432",
		"+ allow new.example.com",
		"- allow old.example.com",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("change list is missing %q:\n%s", want, got)
		}
	}

	// The point of the diff is that nothing else is in it. A domain the user
	// already approved must not be re-listed, and neither must the section
	// headings of the full summary.
	for _, unwanted := range []string{
		"allow github.com", // unchanged rule
		"resources",        // unchanged, and never set
		"mounts\n",         // full-summary heading
		"(reachable on 127.0.0.1 only)",
	} {
		if strings.Contains(got, unwanted) {
			t.Errorf("change list mentions unchanged item %q:\n%s", unwanted, got)
		}
	}
}

func TestAllowReportsRenamedMountAsRemoveAndAdd(t *testing.T) {
	e := testEnv(t)
	shared := t.TempDir()
	project := newProject(t, "version: 1\nimage: img\nmounts:\n  - name: before\n    host_path: "+shared+"\n")
	allowOnce(t, e, project)

	// The guest path changes with the name, so this is not a cosmetic edit even
	// though the host directory is identical.
	rewriteConfig(t, project, "version: 1\nimage: img\nmounts:\n  - name: after\n    host_path: "+shared+"\n")
	got := allowOnce(t, e, project)

	if !strings.Contains(got, "- mount before") || !strings.Contains(got, "+ mount after") {
		t.Errorf("a renamed mount should read as a removal and an addition:\n%s", got)
	}
}

func TestAllowSaysWhenAnEditGrantsNothingNew(t *testing.T) {
	e := testEnv(t)
	project := newProject(t, sampleConfig)
	allowOnce(t, e, project)

	// The hash changes, so trust lapses and plasticturtle allow must still prompt — but
	// the user deserves to be told the edit was inert.
	rewriteConfig(t, project, sampleConfig+"# a comment\n")

	var out bytes.Buffer
	if err := runAllow(e, project, strings.NewReader("y\n"), &out); err != nil {
		t.Fatalf("runAllow: %v\n%s", err, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "nothing it grants has changed") {
		t.Errorf("a comment-only edit should say so:\n%s", got)
	}
	if !strings.Contains(got, "Allow it?") {
		t.Errorf("a changed file must still be re-approved:\n%s", got)
	}
}

func TestAllowOnAnUnchangedConfigDoesNotPrompt(t *testing.T) {
	e := testEnv(t)
	project := newProject(t, sampleConfig)
	allowOnce(t, e, project)

	var out bytes.Buffer
	// Empty stdin: a prompt here would read EOF and report a decline.
	if err := runAllow(e, project, strings.NewReader(""), &out); err != nil {
		t.Fatalf("re-allowing an unchanged config: %v\n%s", err, out.String())
	}
	got := out.String()
	if strings.Contains(got, "Allow it?") {
		t.Errorf("prompted again for a byte-identical config:\n%s", got)
	}
	if !strings.Contains(got, "Unchanged") {
		t.Errorf("expected an 'unchanged' notice:\n%s", got)
	}
}

func TestAllowFallsBackToFullSummaryWithoutASnapshot(t *testing.T) {
	e := testEnv(t)
	project := newProject(t, sampleConfig)
	// A record written by a pt that predates snapshots: hash only.
	if err := e.Trust.Allow(project, config.HashBytes([]byte(sampleConfig)), nil, time.Now()); err != nil {
		t.Fatal(err)
	}

	rewriteConfig(t, project, "version: 1\nimage: other\n")
	got := allowOnce(t, e, project)

	// With nothing to diff against, showing everything is the only honest
	// option — silently showing an empty change list would be the failure.
	for _, want := range []string{"other", "mounts", "ports", "network"} {
		if !strings.Contains(got, want) {
			t.Errorf("fallback summary is missing %q:\n%s", want, got)
		}
	}
}

func TestAllowSummaryShowsNetworkPolicy(t *testing.T) {
	e := testEnv(t)

	restricted := newProject(t, "version: 1\nimage: img\nnetwork:\n  policy: restricted\n  allow:\n    - github.com\n")
	got := allowOnce(t, e, restricted)
	if !strings.Contains(got, "restricted") || !strings.Contains(got, "github.com") {
		t.Errorf("summary hides the egress policy:\n%s", got)
	}

	// Restricted with no rules denies everything; that must not look like an
	// omission from the summary.
	sealed := newProject(t, "version: 1\nimage: img\nnetwork:\n  policy: restricted\n")
	got = allowOnce(t, e, sealed)
	if !strings.Contains(got, "all outbound is denied") {
		t.Errorf("an empty allowlist should be spelled out:\n%s", got)
	}

	open := newProject(t, "version: 1\nimage: img\n")
	got = allowOnce(t, e, open)
	if !strings.Contains(got, "unrestricted outbound") {
		t.Errorf("the default policy should be stated, not implied:\n%s", got)
	}
}

func TestGrantsCoverEveryConfiguredField(t *testing.T) {
	// A config that exercises every field, to catch a new one being added to
	// the schema without reaching the approval prompt.
	cfg := &config.Config{
		Version: config.SchemaVersion,
		Image:   "img",
		// Resources, Ports, Mounts, Network all populated.
		Resources: &config.Resources{CPU: 4, Memory: 8192},
		Ports:     []config.Port{{VMPort: 3000}},
		Mounts:    []config.Mount{{Name: "data", HostPath: "/tmp", Mode: config.ModeRO}},
		Network:   &config.Network{Policy: config.NetRestricted, Allow: []string{"github.com"}},
	}
	gs := grantsOf(cfg, "/proj")

	seen := map[grantKind]bool{}
	for _, g := range gs {
		seen[g.kind] = true
	}
	for _, k := range []grantKind{grantImage, grantResources, grantMount, grantPort, grantNetPolicy, grantNetAllow} {
		if !seen[k] {
			t.Errorf("no grant of kind %s", k)
		}
	}

	// The project mount is implicit and must be granted whether or not the file
	// lists it, since it is the directory the agent actually runs in.
	var project bool
	for _, g := range gs {
		if g.kind == grantMount && g.name == config.ProjectMountName && g.value == "/proj" {
			project = true
		}
	}
	if !project {
		t.Errorf("the implicit project mount is missing from the grants:\n%+v", gs)
	}
}

func TestDiffGrantsIgnoresDomainCasing(t *testing.T) {
	// Normalization happens before comparison, so recasing a rule the firewall
	// will treat identically must not be reported as a change the user has to
	// review.
	old := grantsOf(&config.Config{Image: "img", Network: &config.Network{Policy: config.NetRestricted, Allow: []string{"GitHub.com"}}}, "/proj")
	cur := grantsOf(&config.Config{Image: "img", Network: &config.Network{Policy: config.NetRestricted, Allow: []string{"github.com."}}}, "/proj")
	if got := diffGrants(old, cur); len(got) != 0 {
		t.Errorf("recasing a domain reported changes: %+v", got)
	}
}

func TestDiffGrantsOrdersByKind(t *testing.T) {
	old := grantsOf(&config.Config{Image: "a"}, "/proj")
	cur := grantsOf(&config.Config{
		Image:  "b",
		Ports:  []config.Port{{VMPort: 3000}},
		Mounts: []config.Mount{{Name: "data", HostPath: "/tmp"}},
	}, "/proj")

	got := diffGrants(old, cur)
	var kinds []grantKind
	for _, c := range got {
		kinds = append(kinds, c.kind)
	}
	want := []grantKind{grantImage, grantMount, grantPort}
	if len(kinds) != len(want) {
		t.Fatalf("changes = %+v, want one per kind in %v", got, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Errorf("change %d is %s, want %s (order: %v)", i, kinds[i], want[i], kinds)
		}
	}
}

func TestHumanizeSince(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name string
		at   time.Time
		want string
	}{
		{"zero", time.Time{}, "at some point"},
		{"future", now.Add(time.Hour), "at some point"},
		{"seconds", now.Add(-5 * time.Second), "moments ago"},
		{"one minute", now.Add(-time.Minute - time.Second), "1 minute ago"},
		{"minutes", now.Add(-40 * time.Minute), "40 minutes ago"},
		{"hours", now.Add(-3 * time.Hour), "3 hours ago"},
		{"days", now.Add(-72 * time.Hour), "3 days ago"},
		{"months", now.Add(-90 * 24 * time.Hour), "3 months ago"},
	} {
		if got := humanizeSince(tc.at); got != tc.want {
			t.Errorf("%s: humanizeSince = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestAllowIgnoresATamperedSnapshot(t *testing.T) {
	e := testEnv(t)
	project := newProject(t, sampleConfig)
	allowOnce(t, e, project)

	// Rewrite the stored snapshot so it no longer matches the hash it was
	// recorded against. Trust itself is unaffected (the hash still decides),
	// but the diff must not be built from bytes that cannot be shown to be the
	// ones the user approved.
	dbPath := filepath.Join(stateRootOverride, "trust.json")
	raw, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var db map[string]map[string]any
	if err := json.Unmarshal(raw, &db); err != nil {
		t.Fatal(err)
	}
	rec, ok := db[project]
	if !ok {
		t.Fatalf("no trust record for %s in %s", project, raw)
	}
	rec["raw"] = base64.StdEncoding.EncodeToString([]byte("version: 1\nimage: something-else\n"))
	patched, err := json.Marshal(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath, patched, 0o600); err != nil {
		t.Fatal(err)
	}

	rewriteConfig(t, project, "version: 1\nimage: img\n")
	got := allowOnce(t, e, project)

	if strings.Contains(got, "Changed since") {
		t.Errorf("diffed against a snapshot that does not match its hash:\n%s", got)
	}
	if !strings.Contains(got, "mounts") {
		t.Errorf("expected the full summary as a fallback:\n%s", got)
	}
}
