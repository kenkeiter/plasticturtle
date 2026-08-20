package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/huh"

	"github.com/kenkeiter/plasticturtle/internal/config"
	"github.com/kenkeiter/plasticturtle/internal/progname"
	"github.com/kenkeiter/plasticturtle/internal/tart"
)

// defaultImage is the image pt suggests. It is the one this tool is developed
// and tested against.
const defaultImage = "ghcr.io/cirruslabs/macos-tahoe-base:latest"

// customImageSentinel marks the picker entry that opens a free-text prompt. It
// is not a legal image reference, so it cannot collide with a real one.
const customImageSentinel = "\x00enter-a-reference"

// runInit writes a .plasticturtle interactively and records trust in it.
//
// The file it produces is trusted without a confirmation prompt: the user just
// authored it, answering every question that plasticturtle allow would have asked. Asking
// them to re-approve their own answers would teach them that the trust prompt
// is a formality.
func runInit(e *env, path string, out io.Writer, interactive bool) error {
	dir, err := initTargetDir(path)
	if err != nil {
		return err
	}
	target := filepath.Join(dir, config.FileName)

	if _, err := os.Stat(target); err == nil {
		return fmt.Errorf("%s already exists\nedit it, then run: %s allow", target, progname.Get())
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", target, err)
	}

	if !interactive {
		// Every question below needs an answer. Hanging on a read that will
		// never come is worse than saying so.
		return fmt.Errorf("%s init is interactive; run it from a terminal", progname.Get())
	}

	image, err := pickImage(e)
	if err != nil {
		return err
	}
	ports, err := promptPorts()
	if err != nil {
		return err
	}
	network, err := promptNetwork()
	if err != nil {
		return err
	}

	cfg := &config.Config{Version: config.SchemaVersion, Image: image, Ports: ports, Network: network}
	if err := cfg.Validate(); err != nil {
		return err
	}
	body, err := config.Template(cfg, time.Now())
	if err != nil {
		return err
	}
	if err := os.WriteFile(target, body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", target, err)
	}

	// Trust is keyed on the exact bytes just written, at the same canonical path
	// config.Find will produce later.
	if err := e.Trust.Allow(dir, config.HashBytes(body), body, time.Now()); err != nil {
		return fmt.Errorf("wrote %s but could not record trust: %w", target, err)
	}

	fmt.Fprintf(out, "Wrote %s and allowed it.\n\n", target)
	fmt.Fprintf(out, "Next: %s shell\n", progname.Get())
	fmt.Fprintf(out, "Tip:  add `source <(%s zsh-hook)` to ~/.zshrc for a trust warning and prompt marker.\n", progname.Get())
	return nil
}

// initTargetDir canonicalizes the target the same way config.Find will, so the
// trust entry written here is found by the same key later. A project reached
// through a symlink must be one project, not two.
func initTargetDir(path string) (string, error) {
	if path == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		path = wd
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", abs, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", resolved)
	}
	return resolved, nil
}

// pickImage offers the images tart already has, plus free-text entry for any
// OCI reference.
func pickImage(e *env) (string, error) {
	opts := []huh.Option[string]{}
	for _, name := range availableImages(e) {
		opts = append(opts, huh.NewOption(name, name))
	}
	if len(opts) == 0 {
		// No local images: the default is still a valid answer, it just has to
		// be pulled on first boot.
		opts = append(opts, huh.NewOption(defaultImage+" (will be pulled)", defaultImage))
	}
	opts = append(opts, huh.NewOption("Enter an OCI reference…", customImageSentinel))

	var choice string
	form := huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("Image").
			Description("The VM image this project boots. Cloned fresh for every session group.").
			Options(opts...).
			Value(&choice),
	))
	if err := form.Run(); err != nil {
		return "", err
	}
	if choice != customImageSentinel {
		return choice, nil
	}

	var custom string
	entry := huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title("OCI reference").
			Placeholder(defaultImage).
			Value(&custom).
			Validate(func(s string) error {
				if strings.TrimSpace(s) == "" {
					return fmt.Errorf("an image is required")
				}
				return nil
			}),
	))
	if err := entry.Run(); err != nil {
		return "", err
	}
	return strings.TrimSpace(custom), nil
}

// availableImages lists what tart already has locally, newest names sorted so
// the picker is stable between runs.
func availableImages(e *env) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	vms, err := e.Tart.List(ctx)
	if err != nil {
		// A missing or broken tart is not a reason to refuse to write a config;
		// the user can still type a reference and find out at plasticturtle shell time.
		return nil
	}
	var names []string
	for _, vm := range vms {
		// Never offer one of our own ephemeral clones as a base image.
		if strings.HasPrefix(vm.Name, "pt-") {
			continue
		}
		// Only stopped images are usable as a clone source.
		if vm.State == tart.StateRunning {
			continue
		}
		names = append(names, vm.Name)
	}
	sort.Strings(names)
	return names
}

// promptPorts asks for forwards as a single line, which is friendlier than an
// unbounded repeat loop and keeps the parse testable on its own.
func promptPorts() ([]config.Port, error) {
	var line string
	form := huh.NewForm(huh.NewGroup(
		huh.NewInput().
			Title("Ports (optional)").
			Description("Forwards as vm_port[:host_port], comma separated. Example: 3000, 5432:15432").
			Value(&line).
			Validate(func(s string) error {
				_, err := parsePortSpecs(s)
				return err
			}),
	))
	if err := form.Run(); err != nil {
		return nil, err
	}
	return parsePortSpecs(line)
}

// promptNetwork asks for the outbound network posture and, under a restricted
// policy, the domains to allow. Choosing "open" returns a nil *Network: open is
// the default a nil block already means, so the written file keeps the tidy
// commented example rather than an explicit `policy: open`.
func promptNetwork() (*config.Network, error) {
	const restricted = "restricted"
	var policy string
	form := huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("Network").
			Description("Outbound access from the guest. Restricted is default-deny: only the domains you list are reachable, and tools that ignore it simply get no connectivity.").
			Options(
				huh.NewOption("Open — full internet and LAN access", string(config.NetOpen)),
				huh.NewOption("Restricted — only domains you allow", restricted),
			).
			Value(&policy),
	))
	if err := form.Run(); err != nil {
		return nil, err
	}
	if policy != restricted {
		return nil, nil
	}

	var lines string
	entry := huh.NewForm(huh.NewGroup(
		huh.NewText().
			Title("Allowed domains").
			Description("One per line (or comma separated). A leading *. matches any subdomain. Example: github.com, *.githubusercontent.com").
			Value(&lines).
			Validate(func(s string) error {
				_, err := parseDomainList(s)
				return err
			}),
	))
	if err := entry.Run(); err != nil {
		return nil, err
	}
	allow, err := parseDomainList(lines)
	if err != nil {
		return nil, err
	}
	// An empty allowlist under a restricted policy is valid (it denies all
	// egress), so it is not rejected here — the description says as much.
	return &config.Network{Policy: config.NetRestricted, Allow: allow}, nil
}

// parseDomainList splits a free-text field of domain patterns on commas and
// whitespace (so newline-separated and comma-separated both work) and validates
// each through the same grammar the config loader enforces, returning the
// canonical forms. Like parsePortSpecs, it lives apart from the prompt so the
// field validator can call it and the parse is testable on its own.
func parseDomainList(s string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, field := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == '\t' || r == ' '
	}) {
		norm, err := config.NormalizeDomainPattern(field)
		if err != nil {
			return nil, err
		}
		if seen[norm] {
			return nil, fmt.Errorf("%q is listed twice", field)
		}
		seen[norm] = true
		out = append(out, norm)
	}
	return out, nil
}

// parsePortSpecs parses "3000, 5432:15432" into port mappings. An omitted host
// port means "same as the VM port", matching the config file's own default.
//
// It rejects duplicate host ports itself rather than leaving that to
// Config.Validate. The prompt's validator calls this, so catching it here means
// the user is told while the field is still on screen; catching it later means
// runInit aborts after the form is dismissed and every answer — including the
// image choice — is lost.
func parsePortSpecs(s string) ([]config.Port, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	seen := map[int]bool{}
	var out []config.Port
	for _, field := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		vmStr, hostStr, hasHost := strings.Cut(field, ":")
		vm, err := parsePort(vmStr)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", field, err)
		}
		p := config.Port{VMPort: vm}
		effective := vm
		if hasHost {
			host, err := parsePort(hostStr)
			if err != nil {
				return nil, fmt.Errorf("%q: %w", field, err)
			}
			p.HostPort = host
			effective = host
		}
		// Duplicates are checked on the effective host port, so that "3000" and
		// "9000:3000" collide the same way Config.Validate would see them.
		if seen[effective] {
			return nil, fmt.Errorf("host port %d is claimed twice", effective)
		}
		seen[effective] = true
		out = append(out, p)
	}
	return out, nil
}

func parsePort(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("not a port number")
	}
	if n < 1 || n > 65535 {
		return 0, fmt.Errorf("port %d is out of range 1..65535", n)
	}
	return n, nil
}
