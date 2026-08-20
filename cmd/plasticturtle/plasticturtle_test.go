package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kenkeiter/plasticturtle/internal/config"
	"github.com/kenkeiter/plasticturtle/internal/zshplugin"
)

const sampleConfig = `version: 1
image: ghcr.io/cirruslabs/macos-tahoe-base:latest
ports:
  - vm_port: 3000
  - vm_port: 5432
    host_port: 15432
`

// testEnv points pt at a temp state root, so no test touches the developer's
// real trust database or instance records.
func testEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root) // for code paths that call state.DefaultRoot

	prev := stateRootOverride
	stateRootOverride = filepath.Join(root, "plasticturtle")
	t.Cleanup(func() { stateRootOverride = prev })

	e, err := openEnv()
	if err != nil {
		t.Fatalf("openEnv: %v", err)
	}
	return e
}

// newProject writes a project directory containing body.
func newProject(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resolved, config.FileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestCheckTrustExitCodes(t *testing.T) {
	// checkTrust reads the state root through state.DefaultRoot, so the env var
	// is what redirects it.
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)

	prev := stateRootOverride
	stateRootOverride = filepath.Join(root, "plasticturtle")
	t.Cleanup(func() { stateRootOverride = prev })

	e, err := openEnv()
	if err != nil {
		t.Fatal(err)
	}

	project := newProject(t, sampleConfig)

	// Never allowed.
	if got := checkTrust(project); got != zshplugin.ExitUntrusted {
		t.Errorf("never-allowed config = %d, want ExitUntrusted(%d)", got, zshplugin.ExitUntrusted)
	}

	// Allowed at its current bytes.
	var out bytes.Buffer
	if err := runAllow(e, project, strings.NewReader("y\n"), &out); err != nil {
		t.Fatalf("runAllow: %v\n%s", err, out.String())
	}
	if got := checkTrust(project); got != zshplugin.ExitTrusted {
		t.Errorf("allowed config = %d, want ExitTrusted(%d)", got, zshplugin.ExitTrusted)
	}

	// Changed after being allowed. This is the case the whole trust model
	// exists for: an agent editing the config must not inherit its approval.
	path := filepath.Join(project, config.FileName)
	if err := os.WriteFile(path, []byte(sampleConfig+"# appended\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := checkTrust(project); got != zshplugin.ExitUntrusted {
		t.Errorf("changed config = %d, want ExitUntrusted(%d)", got, zshplugin.ExitUntrusted)
	}

	// No config at all: neither verdict is honest.
	if got := checkTrust(t.TempDir()); got != zshplugin.ExitError {
		t.Errorf("missing config = %d, want ExitError(%d)", got, zshplugin.ExitError)
	}
}

func TestCheckTrustFromSubdirectory(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	prev := stateRootOverride
	stateRootOverride = filepath.Join(root, "plasticturtle")
	t.Cleanup(func() { stateRootOverride = prev })

	e, err := openEnv()
	if err != nil {
		t.Fatal(err)
	}
	project := newProject(t, sampleConfig)
	var out bytes.Buffer
	if err := runAllow(e, project, strings.NewReader("y\n"), &out); err != nil {
		t.Fatal(err)
	}

	// The plugin calls _check-trust with whatever directory the user cd'd into,
	// which is routinely a subdirectory of the project.
	sub := filepath.Join(project, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := checkTrust(sub); got != zshplugin.ExitTrusted {
		t.Errorf("from subdirectory = %d, want ExitTrusted(%d)", got, zshplugin.ExitTrusted)
	}
}

func TestAllowRequiresConfirmation(t *testing.T) {
	e := testEnv(t)
	project := newProject(t, sampleConfig)

	for _, answer := range []string{"n\n", "\n", "no\n", ""} {
		var out bytes.Buffer
		err := runAllow(e, project, strings.NewReader(answer), &out)
		if err != errDeclined {
			t.Errorf("answer %q: err = %v, want errDeclined", answer, err)
		}
		// Declining must not record anything, including on EOF.
		if got := checkTrust(project); got == zshplugin.ExitTrusted {
			t.Errorf("answer %q recorded trust anyway", answer)
		}
	}
}

func TestAllowSummaryShowsWhatIsGranted(t *testing.T) {
	e := testEnv(t)
	// A read-write mount of a directory outside the project is the most
	// dangerous thing a config can express, so it is what the summary must make
	// unmissable.
	extra := t.TempDir()
	body := "version: 1\nimage: img\nmounts:\n  - name: data\n    host_path: " + extra + "\n    mode: rw\n"
	project := newProject(t, body)

	var out bytes.Buffer
	if err := runAllow(e, project, strings.NewReader("y\n"), &out); err != nil {
		t.Fatalf("runAllow: %v\n%s", err, out.String())
	}
	got := out.String()

	for _, want := range []string{
		"img",                   // the image
		extra,                   // the absolute host path, not "./data"
		"READ-WRITE",            // spelled out; "rw" understates it
		config.ProjectMountName, // the always-present project mount
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary does not mention %q:\n%s", want, got)
		}
	}
}

func TestAllowRefusesInvalidConfigBeforePrompting(t *testing.T) {
	e := testEnv(t)
	project := newProject(t, "version: 2\nimage: img\n") // wrong version

	var out bytes.Buffer
	// Stdin is empty: if the implementation prompted, it would read EOF and
	// report a decline rather than a validation failure.
	err := runAllow(e, project, strings.NewReader(""), &out)
	if err == nil {
		t.Fatal("an invalid config was allowed")
	}
	if err == errDeclined {
		t.Fatal("prompted before validating; an invalid config cannot be trusted")
	}
	if strings.Contains(out.String(), "Allow it?") {
		t.Errorf("prompted for an invalid config:\n%s", out.String())
	}
}

func TestInitRefusesExistingConfig(t *testing.T) {
	e := testEnv(t)
	project := newProject(t, sampleConfig)

	var out bytes.Buffer
	err := runInit(e, project, &out, true)
	if err == nil {
		t.Fatal("plasticturtle init overwrote an existing .plasticturtle")
	}
	if !strings.Contains(err.Error(), "plasticturtle allow") {
		t.Errorf("error should point at plasticturtle allow, got: %v", err)
	}
}

func TestInitRefusesNonInteractive(t *testing.T) {
	e := testEnv(t)
	var out bytes.Buffer
	// Every question plasticturtle init asks needs an answer; hanging on a read that will
	// never arrive is worse than saying so.
	err := runInit(e, t.TempDir(), &out, false)
	if err == nil || !strings.Contains(err.Error(), "interactive") {
		t.Errorf("err = %v, want an interactivity complaint", err)
	}
}

func TestParsePortSpecs(t *testing.T) {
	tests := []struct {
		in      string
		want    []config.Port
		wantErr bool
	}{
		{in: "", want: nil},
		{in: "   ", want: nil},
		{in: "3000", want: []config.Port{{VMPort: 3000}}},
		{in: "5432:15432", want: []config.Port{{VMPort: 5432, HostPort: 15432}}},
		{in: "3000, 5432:15432", want: []config.Port{{VMPort: 3000}, {VMPort: 5432, HostPort: 15432}}},
		{in: "3000 8080", want: []config.Port{{VMPort: 3000}, {VMPort: 8080}}},
		{in: "not-a-port", wantErr: true},
		{in: "0", wantErr: true},
		{in: "65536", wantErr: true},
		{in: "3000:0", wantErr: true},
		// Caught here rather than by Config.Validate, so plasticturtle init can re-prompt
		// instead of discarding every answer after the form is dismissed.
		{in: "3000, 3000", wantErr: true},
		{in: "3000, 9000:3000", wantErr: true},
	}
	for _, tc := range tests {
		got, err := parsePortSpecs(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parsePortSpecs(%q) = %v, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parsePortSpecs(%q): %v", tc.in, err)
			continue
		}
		if len(got) != len(tc.want) {
			t.Errorf("parsePortSpecs(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("parsePortSpecs(%q)[%d] = %v, want %v", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

func TestParseDomainList(t *testing.T) {
	tests := []struct {
		in      string
		want    []string
		wantErr bool
	}{
		{in: "", want: nil},
		{in: "  \n ", want: nil},
		{in: "github.com", want: []string{"github.com"}},
		// Newlines (the multiline field's natural separator), commas, and spaces
		// all delimit.
		{in: "github.com\nregistry.npmjs.org", want: []string{"github.com", "registry.npmjs.org"}},
		{in: "github.com, *.githubusercontent.com", want: []string{"github.com", "*.githubusercontent.com"}},
		// Canonicalized to the form the loader stores.
		{in: "GitHub.com", want: []string{"github.com"}},
		{in: "example.com.", want: []string{"example.com"}},
		// Rejected by the shared grammar.
		{in: "https://github.com", wantErr: true},
		{in: "github.com/path", wantErr: true},
		{in: "*.com", wantErr: true},
		{in: "nodot", wantErr: true},
		{in: "10.0.0.1", wantErr: true},
		// Caught here so plasticturtle init can re-prompt rather than discarding answers.
		{in: "github.com, github.com", wantErr: true},
		{in: "github.com, GitHub.com.", wantErr: true},
	}
	for _, tc := range tests {
		got, err := parseDomainList(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseDomainList(%q) = %v, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseDomainList(%q): %v", tc.in, err)
			continue
		}
		if len(got) != len(tc.want) {
			t.Errorf("parseDomainList(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("parseDomainList(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

func TestListJSONIsAnArrayWhenEmpty(t *testing.T) {
	e := testEnv(t)
	var out bytes.Buffer
	if err := runList(e, &out, true); err != nil {
		t.Fatalf("runList: %v", err)
	}
	// An empty result must be [] and not null: anything piping this into jq
	// should not have to special-case the no-instances case.
	var rows []listRow
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "[]") {
		t.Errorf("empty list --json = %q, want []", out.String())
	}
}

// The empty store prints a sentence rather than an empty table. The DISK*
// label and footnote are asserted in list_test.go, where there is a row to
// render -- with zero rows neither is ever emitted.
func TestListWithNoInstances(t *testing.T) {
	e := testEnv(t)
	var out bytes.Buffer
	if err := runList(e, &out, false); err != nil {
		t.Fatalf("runList: %v", err)
	}
	if !strings.Contains(out.String(), "No active instances") {
		t.Errorf("unexpected output: %s", out.String())
	}
}

func TestPortsShowsConfiguredForwardsWhenInactive(t *testing.T) {
	e := testEnv(t)
	project := newProject(t, sampleConfig)

	var out bytes.Buffer
	if err := runPorts(e, project, &out, false, false); err != nil {
		t.Fatalf("runPorts: %v", err)
	}
	got := out.String()
	// With no instance, every configured forward is inactive but still listed:
	// the user asked what this project forwards, not what is running.
	for _, want := range []string{"3000", "15432", "inactive"} {
		if !strings.Contains(got, want) {
			t.Errorf("ports output missing %q:\n%s", want, got)
		}
	}
}

func TestPortsJSONIsValid(t *testing.T) {
	e := testEnv(t)
	project := newProject(t, sampleConfig)

	var out bytes.Buffer
	if err := runPorts(e, project, &out, false, true); err != nil {
		t.Fatalf("runPorts: %v", err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("ports --json is not valid JSON: %v\n%s", err, out.String())
	}
	if len(rows) != 2 {
		t.Errorf("got %d rows, want 2:\n%s", len(rows), out.String())
	}
}

func TestSuperviseRejectsMalformedStdin(t *testing.T) {
	for _, in := range []string{"", "{", "null", `{"projectId":"x"}`, `{"nope":1}`} {
		var out bytes.Buffer
		if err := runSupervise(strings.NewReader(in), &out); err == nil {
			t.Errorf("runSupervise(%q) accepted malformed parameters", in)
		}
	}
}

func TestZSHHookPrintsPlugin(t *testing.T) {
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"zsh-hook"})
	if err := root.Execute(); err != nil {
		t.Fatalf("zsh-hook: %v", err)
	}
	if out.String() != zshplugin.Script() {
		t.Error("zsh-hook output does not match the embedded plugin")
	}
}

func TestHumanBytes(t *testing.T) {
	tests := []struct {
		in   uint64
		want string
	}{
		{0, "0B"},
		{512, "512B"},
		{2048, "2.0K"},
		{5 * 1024 * 1024, "5.0M"},
		{3 * 1024 * 1024 * 1024, "3.0G"},
	}
	for _, tc := range tests {
		if got := humanBytes(tc.in); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
