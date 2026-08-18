package zshplugin

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestScriptIsEmbedded(t *testing.T) {
	s := Script()
	if strings.TrimSpace(s) == "" {
		t.Fatal("Script() is empty; the embed did not pick up pt.plugin.zsh")
	}

	// These are the contract with spec 5.1 and with cmd/pt. A refactor that
	// drops one of them is a silent regression in somebody's shell.
	for _, want := range []string{
		"command -v pt",                   // silent no-op when pt is absent
		"add-zsh-hook",                    // must not clobber the user's hook arrays
		"PT_PROMPT",                       // the exported prompt segment
		"_check-trust",                    // the trust probe
		"is not allowed (new or changed)", // the exact warning text
	} {
		if !strings.Contains(s, want) {
			t.Errorf("Script() is missing %q", want)
		}
	}
}

// zshPath finds zsh, or skips. Every test below needs a real shell: a plugin
// that is only string-compared is a plugin nobody has run.
func zshPath(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh not available")
	}
	return p
}

func TestScriptParses(t *testing.T) {
	zsh := zshPath(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "pt.plugin.zsh")
	if err := os.WriteFile(path, []byte(Script()), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := exec.Command(zsh, "-n", path).CombinedOutput()
	if err != nil {
		t.Fatalf("zsh -n rejected the plugin: %v\n%s", err, out)
	}
}

// harness builds a temp world: a stub `pt` that exits with the code we want, a
// project directory containing a .plasticturtle, and the plugin on disk.
type harness struct {
	dir     string // temp root; stands in for "above $HOME"
	home    string // $HOME for the test shell
	project string
	plugin  string
	binDir  string
}

func newHarness(t *testing.T, exitCode int) harness {
	t.Helper()
	dir := t.TempDir()

	h := harness{
		dir:     dir,
		home:    filepath.Join(dir, "home"),
		project: filepath.Join(dir, "home", "proj"),
		plugin:  filepath.Join(dir, "pt.plugin.zsh"),
		binDir:  filepath.Join(dir, "bin"),
	}
	for _, d := range []string{h.project, h.binDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(h.project, ".plasticturtle"), []byte("version: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.plugin, []byte(Script()), 0o644); err != nil {
		t.Fatal(err)
	}
	// The stub stands in for the real binary: the plugin's whole contract with
	// pt is an exit code, so an exit code is all the stub needs to produce.
	stub := "#!/bin/sh\nexit " + itoa(exitCode) + "\n"
	if err := os.WriteFile(filepath.Join(h.binDir, "pt"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	return h
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	digits := ""
	for i > 0 {
		digits = string(rune('0'+i%10)) + digits
		i /= 10
	}
	return digits
}

// run sources the plugin in a non-interactive zsh and executes body.
func (h harness) run(t *testing.T, body string) string {
	t.Helper()
	zsh := zshPath(t)

	script := "set -e\n" +
		"source " + h.plugin + "\n" +
		body + "\n"

	cmd := exec.Command(zsh, "-c", script)
	cmd.Dir = h.home
	// HOME is a directory inside the temp root, so the upward walk's $HOME
	// bound is exercised against a real boundary rather than the developer's
	// home -- and so a test can place a config above it without touching
	// anything shared.
	cmd.Env = append(os.Environ(),
		"PATH="+h.binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HOME="+h.home,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("zsh failed: %v\n%s", err, out)
	}
	return string(out)
}

func TestUntrustedProjectWarnsAndMarksPromptYellow(t *testing.T) {
	h := newHarness(t, ExitUntrusted)
	out := h.run(t, `cd proj; print -r -- "PROMPT_SEGMENT=$PT_PROMPT"`)

	if !strings.Contains(out, "is not allowed (new or changed)") {
		t.Errorf("no warning for an untrusted project:\n%s", out)
	}
	if !strings.Contains(out, "PROMPT_SEGMENT=") || !strings.Contains(out, "yellow") {
		t.Errorf("prompt segment is not yellow for an untrusted project:\n%s", out)
	}
}

func TestTrustedProjectIsSilentAndMarksPromptGreen(t *testing.T) {
	h := newHarness(t, ExitTrusted)
	out := h.run(t, `cd proj; print -r -- "PROMPT_SEGMENT=$PT_PROMPT"`)

	if strings.Contains(out, "is not allowed") {
		t.Errorf("warned about a trusted project:\n%s", out)
	}
	if !strings.Contains(out, "green") {
		t.Errorf("prompt segment is not green for a trusted project:\n%s", out)
	}
}

func TestCheckFailureShowsNoSegment(t *testing.T) {
	// pt failed for a reason of its own; claiming either trust state would be a
	// lie, so the plugin must claim neither.
	h := newHarness(t, ExitError)
	out := h.run(t, `cd proj; print -r -- "SEGMENT=[$PT_PROMPT]"`)

	if !strings.Contains(out, "SEGMENT=[]") {
		t.Errorf("expected an empty segment when the check errors:\n%s", out)
	}
	if strings.Contains(out, "is not allowed") {
		t.Errorf("warned on an error exit, which is not an untrusted verdict:\n%s", out)
	}
}

func TestLeavingProjectClearsSegment(t *testing.T) {
	h := newHarness(t, ExitTrusted)
	out := h.run(t, `cd proj; cd ..; print -r -- "SEGMENT=[$PT_PROMPT]"`)

	if !strings.Contains(out, "SEGMENT=[]") {
		t.Errorf("segment survived leaving the project:\n%s", out)
	}
}

func TestDoubleSourcingDoesNotDoubleRegister(t *testing.T) {
	h := newHarness(t, ExitTrusted)
	// Prompt frameworks re-source their fragments, so this is the realistic
	// case, not a pathological one.
	out := h.run(t, `source `+h.plugin+`
cd proj
print -r -- "CHPWD=${#chpwd_functions} PRECMD=${#precmd_functions}"`)

	if !strings.Contains(out, "CHPWD=1") || !strings.Contains(out, "PRECMD=1") {
		t.Errorf("double-sourcing registered the hooks more than once:\n%s", out)
	}
}

func TestPromptPrefixIsNotDuplicatedAcrossRenders(t *testing.T) {
	h := newHarness(t, ExitTrusted)
	// _pt_precmd runs before every prompt; the segment must appear once no
	// matter how many times it has run.
	out := h.run(t, `cd proj
PROMPT='$ '
_pt_precmd; _pt_precmd; _pt_precmd
print -r -- "PROMPT=[$PROMPT]"`)

	if n := strings.Count(out, "🐢"); n != 1 {
		t.Errorf("turtle appears %d times in PROMPT after three renders, want 1:\n%s", n, out)
	}
}

func TestMissingPTIsSilent(t *testing.T) {
	zsh := zshPath(t)
	dir := t.TempDir()
	plugin := filepath.Join(dir, "pt.plugin.zsh")
	if err := os.WriteFile(plugin, []byte(Script()), 0o644); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "emptybin")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(zsh, "-c", "source "+plugin+"; print -r -- DONE")
	cmd.Dir = dir
	// A PATH with no pt at all: sourcing from .zshrc on a machine without pt
	// must not produce an error on every new shell.
	cmd.Env = []string{"PATH=" + empty, "HOME=" + dir}

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sourcing without pt on PATH failed: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "DONE" {
		t.Errorf("sourcing without pt produced output %q, want silence", got)
	}
}

func TestWalkStopsAtHome(t *testing.T) {
	h := newHarness(t, ExitTrusted)
	// A .plasticturtle above $HOME must not be found: the walk is bounded so a
	// shell deep in a home directory does not stat its way to the root on every
	// cd. The marker goes just above $HOME, still inside the temp root.
	marker := filepath.Join(h.dir, ".plasticturtle")
	if err := os.WriteFile(marker, []byte("version: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := h.run(t, `print -r -- "SEGMENT=[$PT_PROMPT]"`)
	if !strings.Contains(out, "SEGMENT=[]") {
		t.Errorf("walk escaped $HOME and found a config above it:\n%s", out)
	}
}
