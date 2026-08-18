// Package config parses and validates the .plasticturtle project file.
//
// This is a security-relevant file: it names the image a project boots, the
// host directories exposed to it, and the ports opened on the host. Decoding
// is therefore strict (unknown keys are errors) and validation is total —
// every field is checked before any of it reaches tart.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// FileName is the per-project config file, expected at the project root.
const FileName = ".plasticturtle"

// SchemaVersion is the only accepted value of the version field.
const SchemaVersion = 1

// ProjectMountName is the reserved mounts[] entry that overrides the mode of
// the always-present project directory share. It is the one mount for which
// host_path is forbidden rather than required.
const ProjectMountName = "project"

// GuestSharesRoot is where virtiofs shares appear on macOS guests. Linux
// guests must mount them manually; see the README.
const GuestSharesRoot = "/Volumes/My Shared Files"

// Sentinel errors. Callers distinguish these to produce the right user-facing
// message: "no .plasticturtle found" is a different failure from "your
// .plasticturtle is malformed".
var (
	// ErrNotFound means no .plasticturtle exists at or above the start dir.
	ErrNotFound = errors.New("no .plasticturtle found")
)

// Mode is a mount's access mode.
type Mode string

const (
	ModeRW Mode = "rw"
	ModeRO Mode = "ro"
)

// Config is the decoded .plasticturtle. Field order matches the documented
// file layout so that emitted files read naturally.
type Config struct {
	Version   int        `yaml:"version"`
	Image     string     `yaml:"image"`
	Resources *Resources `yaml:"resources,omitempty"`
	Ports     []Port     `yaml:"ports,omitempty"`
	Mounts    []Mount    `yaml:"mounts,omitempty"`
}

// Resources overrides the CPU/memory inherited from the image. A zero field
// means "inherit"; the distinction between zero and absent is why Resources is
// a pointer on Config.
type Resources struct {
	CPU    int `yaml:"cpu,omitempty"`
	Memory int `yaml:"memory,omitempty"` // MiB
}

// Port is a VM -> host forward. HostPort zero means "same as VMPort".
type Port struct {
	VMPort   int `yaml:"vm_port"`
	HostPort int `yaml:"host_port,omitempty"`
}

// Mount is a directory share. HostPath is required except for the reserved
// project entry, where it is forbidden. Empty Mode means ModeRW.
type Mount struct {
	Name     string `yaml:"name"`
	HostPath string `yaml:"host_path,omitempty"`
	Mode     Mode   `yaml:"mode,omitempty"`
}

// Resolved is a Config with every default applied and every path made
// absolute: the implicit project mount is materialized, ~ and relative host
// paths are expanded, and omitted host ports are filled in.
//
// Everything downstream of parsing consumes Resolved, never Config. That is
// what keeps path expansion from being reimplemented (differently) in the
// supervisor and the shell.
type Resolved struct {
	// ProjectPath is the canonical absolute project directory.
	ProjectPath string
	Image       string
	CPU         int // 0 = inherit from image
	Memory      int // MiB; 0 = inherit from image
	Mounts      []ResolvedMount
	Ports       []ResolvedPort
}

// ResolvedMount is a mount with an absolute, existing host path.
type ResolvedMount struct {
	Name     string
	HostPath string
	Mode     Mode
}

// ResolvedPort is a forward with both ends specified.
type ResolvedPort struct {
	VMPort   int
	HostPort int
}

// minCPU and minMemoryMiB are the floors from spec §3.1. A VM below either is
// not a mistake tart will catch for us — it boots badly instead.
const (
	minCPU       = 1
	minMemoryMiB = 512
)

// maxPort is the top of the TCP port space; port 0 is excluded deliberately,
// since "let the kernel pick" is not a thing a project file may ask for.
const maxPort = 65535

// mountNameRe is spec §3.1's mount-name grammar. Names become tart --dir
// labels and guest path components, so anything with a separator, a space, or
// a shell metacharacter has to be rejected here rather than escaped later.
var mountNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// Find walks upward from startDir looking for FileName, stopping at the
// filesystem root. It returns the canonical absolute path of the directory
// containing the file, or ErrNotFound.
//
// Canonicalization happens here so that a project reached through a symlink is
// the same project as one reached directly (see spec section 10).
func Find(startDir string) (projectDir string, err error) {
	if startDir == "" {
		startDir = "."
	}
	dir, err := filepath.Abs(startDir)
	if err != nil {
		return "", fmt.Errorf("resolving %q: %w", startDir, err)
	}
	// Canonicalize the starting point before walking: the ancestors of a
	// symlinked directory are the ancestors of its target, and walking the
	// lexical path would search the wrong chain entirely. Best effort — a
	// nonexistent startDir still gets a clean ErrNotFound below.
	if canon, cerr := filepath.EvalSymlinks(dir); cerr == nil {
		dir = canon
	}
	for {
		if st, serr := os.Stat(filepath.Join(dir, FileName)); serr == nil && st.Mode().IsRegular() {
			canon, cerr := filepath.EvalSymlinks(dir)
			if cerr != nil {
				return "", fmt.Errorf("canonicalizing project dir %q: %w", dir, cerr)
			}
			return canon, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir { // filesystem root
			return "", ErrNotFound
		}
		dir = parent
	}
}

// Load reads and strictly decodes the config in projectDir. It returns the
// parsed config alongside the exact bytes read — the bytes are what trust
// hashing operates on, so they must not be re-serialized.
//
// Load does not validate; call Validate.
func Load(projectDir string) (cfg *Config, raw []byte, err error) {
	path := filepath.Join(projectDir, FileName)
	raw, err = os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, fmt.Errorf("reading %s: %w", path, err)
	}
	cfg, err = decode(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, raw, nil
}

// decode strict-parses config bytes. KnownFields(true) turns a typo in a
// security-relevant key ("mount:", "hostport:") into an error instead of a
// silently ignored line.
func decode(raw []byte) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)

	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("contains no configuration")
		}
		return nil, err
	}
	// A second document would be invisible to the reader of the first, which
	// is exactly the kind of hiding place this file must not have.
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, errors.New("contains more than one YAML document")
	} else if !errors.Is(err, io.EOF) {
		return nil, err
	}
	return &cfg, nil
}

// Validate enforces every rule in spec section 3.1. It reports all violations
// it can find, joined, rather than only the first: a user fixing a config
// should not have to run pt allow five times.
func (c *Config) Validate() error {
	if c == nil {
		return errors.New("config is nil")
	}
	var errs []error

	if c.Version != SchemaVersion {
		errs = append(errs, fmt.Errorf("version: must be %d, got %d", SchemaVersion, c.Version))
	}
	if strings.TrimSpace(c.Image) == "" {
		errs = append(errs, errors.New("image: required and must be non-empty"))
	}
	if r := c.Resources; r != nil {
		// Zero means "inherit from the image" (see Resources); only a value the
		// user actually chose can be out of range.
		if r.CPU != 0 && r.CPU < minCPU {
			errs = append(errs, fmt.Errorf("resources.cpu: must be >= %d, got %d", minCPU, r.CPU))
		}
		if r.Memory != 0 && r.Memory < minMemoryMiB {
			errs = append(errs, fmt.Errorf("resources.memory: must be >= %d MiB, got %d", minMemoryMiB, r.Memory))
		}
	}
	errs = append(errs, validatePorts(c.Ports)...)
	errs = append(errs, validateMounts(c.Mounts)...)

	return errors.Join(errs...)
}

// validatePorts checks each forward and then the set as a whole. The duplicate
// check runs on *effective* host ports, after the vm_port default is applied,
// because {vm_port: 3000} and {vm_port: 9000, host_port: 3000} collide on the
// host even though nothing in the file text repeats.
func validatePorts(ports []Port) []error {
	var errs []error
	seen := make(map[int]int, len(ports)) // effective host port -> first index
	for i, p := range ports {
		if p.VMPort < 1 || p.VMPort > maxPort {
			errs = append(errs, fmt.Errorf("ports[%d].vm_port: must be in 1..%d, got %d", i, maxPort, p.VMPort))
		}
		if p.HostPort != 0 && (p.HostPort < 1 || p.HostPort > maxPort) {
			errs = append(errs, fmt.Errorf("ports[%d].host_port: must be in 1..%d, got %d", i, maxPort, p.HostPort))
		}
		host := p.HostPort
		if host == 0 {
			host = p.VMPort
		}
		if host < 1 || host > maxPort {
			continue // already reported; do not let junk poison the dup check
		}
		if first, dup := seen[host]; dup {
			errs = append(errs, fmt.Errorf("ports[%d]: host port %d is already mapped by ports[%d]", i, host, first))
			continue
		}
		seen[host] = i
	}
	return errs
}

// validateMounts enforces the name grammar, uniqueness, and the inverted
// host_path rule for the reserved project entry.
func validateMounts(mounts []Mount) []error {
	var errs []error
	seen := make(map[string]int, len(mounts))
	for i, m := range mounts {
		switch {
		case m.Name == "":
			errs = append(errs, fmt.Errorf("mounts[%d].name: required", i))
		case !mountNameRe.MatchString(m.Name):
			errs = append(errs, fmt.Errorf("mounts[%d].name: %q must match [a-zA-Z0-9_-]+", i, m.Name))
		}
		if first, dup := seen[m.Name]; dup && m.Name != "" {
			errs = append(errs, fmt.Errorf("mounts[%d].name: %q is already used by mounts[%d]", i, m.Name, first))
		} else if m.Name != "" {
			seen[m.Name] = i
		}
		if m.Name == ProjectMountName {
			// The project share's host path is the project itself; letting the
			// file name a different one would let a config silently redirect
			// what "the project" means inside the VM.
			if m.HostPath != "" {
				errs = append(errs, fmt.Errorf("mounts[%d]: host_path is not allowed on the reserved %q mount (its path is the project directory)", i, ProjectMountName))
			}
		} else if strings.TrimSpace(m.HostPath) == "" {
			errs = append(errs, fmt.Errorf("mounts[%d].host_path: required for mount %q", i, m.Name))
		}
		switch m.Mode {
		case "", ModeRW, ModeRO:
		default:
			errs = append(errs, fmt.Errorf("mounts[%d].mode: must be %q or %q, got %q", i, ModeRW, ModeRO, m.Mode))
		}
	}
	return errs
}

// Resolve applies defaults and expands paths against projectDir, which must be
// canonical and absolute. It returns an error if a host path does not exist —
// a missing mount source is a hard error before any VM is cloned.
//
// Resolve assumes Validate has already passed.
func (c *Config) Resolve(projectDir string) (*Resolved, error) {
	if c == nil {
		return nil, errors.New("config is nil")
	}
	if !filepath.IsAbs(projectDir) {
		return nil, fmt.Errorf("project dir %q is not absolute", projectDir)
	}
	projectDir = filepath.Clean(projectDir)

	res := &Resolved{ProjectPath: projectDir, Image: strings.TrimSpace(c.Image)}
	if c.Resources != nil {
		res.CPU, res.Memory = c.Resources.CPU, c.Resources.Memory
	}

	for _, p := range c.Ports {
		host := p.HostPort
		if host == 0 {
			host = p.VMPort
		}
		res.Ports = append(res.Ports, ResolvedPort{VMPort: p.VMPort, HostPort: host})
	}

	// The project share always exists, whether or not the file mentions it, and
	// always comes first so that summaries and tart argv list it first.
	project := ResolvedMount{Name: ProjectMountName, HostPath: projectDir, Mode: ModeRW}
	var errs []error
	for _, m := range c.Mounts {
		if m.Name == ProjectMountName {
			if m.Mode != "" {
				project.Mode = m.Mode
			}
			continue
		}
		host, err := expandHostPath(m.HostPath, projectDir)
		if err != nil {
			errs = append(errs, fmt.Errorf("mount %q: %w", m.Name, err))
			continue
		}
		mode := m.Mode
		if mode == "" {
			mode = ModeRW
		}
		res.Mounts = append(res.Mounts, ResolvedMount{Name: m.Name, HostPath: host, Mode: mode})
	}
	res.Mounts = append([]ResolvedMount{project}, res.Mounts...)

	// Check existence for every share, project included: a project directory
	// that vanished mid-flight is the same failure as a missing dataset dir,
	// and both must surface before anything is cloned.
	for _, m := range res.Mounts {
		st, err := os.Stat(m.HostPath)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			errs = append(errs, fmt.Errorf("mount %q: host path %s does not exist", m.Name, m.HostPath))
		case err != nil:
			errs = append(errs, fmt.Errorf("mount %q: host path %s: %w", m.Name, m.HostPath, err))
		case !st.IsDir():
			errs = append(errs, fmt.Errorf("mount %q: host path %s is not a directory", m.Name, m.HostPath))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return res, nil
}

// expandHostPath turns a config-authored path into an absolute one: ~ is the
// invoking user's home, and anything relative is relative to the project, not
// to whatever directory pt happened to be invoked from.
func expandHostPath(p, projectDir string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", errors.New("host_path is empty")
	}
	if p == "~" || strings.HasPrefix(p, "~"+string(filepath.Separator)) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expanding ~: %w", err)
		}
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	} else if strings.HasPrefix(p, "~") {
		// ~otheruser is deliberately unsupported rather than silently treated
		// as a relative directory named "~otheruser".
		return "", fmt.Errorf("%q: ~user paths are not supported; use an absolute path", p)
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(projectDir, p)
	}
	return filepath.Clean(p), nil
}

// HashBytes returns the trust hash of a config file's exact bytes, formatted
// "sha256:<hex>".
func HashBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// GuestProjectPath is where the project share appears inside a macOS guest.
func GuestProjectPath() string { return GuestSharesRoot + "/" + ProjectMountName }

// Template renders a commented .plasticturtle for pt init. The comments are
// part of the deliverable: this file is meant to be human-edited later.
func Template(c *Config, generatedAt time.Time) ([]byte, error) {
	if c == nil {
		return nil, errors.New("config is nil")
	}
	// pt init builds the Config from its picker and need not know the schema
	// version, but everything else must already be valid — writing a file that
	// the very next load rejects would be a worse first run than failing here.
	check := *c
	check.Version = SchemaVersion
	if err := check.Validate(); err != nil {
		return nil, fmt.Errorf("refusing to write an invalid %s: %w", FileName, err)
	}

	var b strings.Builder
	b.WriteString("# " + FileName + " — Plastic Turtle project config.\n")
	b.WriteString("# Checked into the repo, human-edited. After any change, run `pt allow`:\n")
	b.WriteString("# this file is inert until its exact contents are trusted.\n")
	if !generatedAt.IsZero() {
		b.WriteString("# Generated by `pt init` on " + generatedAt.UTC().Format(time.RFC3339) + ".\n")
	}
	fmt.Fprintf(&b, "version: %d\n", SchemaVersion)

	b.WriteString("\n# Required. Tart image to clone (local image name or OCI reference).\n")
	b.WriteString("image: " + yamlScalar(check.Image) + "\n")

	b.WriteString("\n# Optional. Override the CPU/memory inherited from the image.\n")
	if r := check.Resources; r != nil && (r.CPU != 0 || r.Memory != 0) {
		b.WriteString("resources:\n")
		if r.CPU != 0 {
			fmt.Fprintf(&b, "  cpu: %d          # vCPUs\n", r.CPU)
		}
		if r.Memory != 0 {
			fmt.Fprintf(&b, "  memory: %d    # MiB\n", r.Memory)
		}
	} else {
		b.WriteString("#resources:\n#  cpu: 8          # vCPUs\n#  memory: 8192    # MiB\n")
	}

	b.WriteString("\n# Optional. Port forwards, VM -> host, bound on 127.0.0.1.\n")
	b.WriteString("# host_port may be omitted, in which case it equals vm_port.\n")
	if len(check.Ports) > 0 {
		b.WriteString("ports:\n")
		for _, p := range check.Ports {
			if p.HostPort != 0 {
				fmt.Fprintf(&b, "  - vm_port: %d\n    host_port: %d\n", p.VMPort, p.HostPort)
			} else {
				fmt.Fprintf(&b, "  - vm_port: %d          # host_port defaults to %d\n", p.VMPort, p.VMPort)
			}
		}
	} else {
		b.WriteString("#ports:\n#  - vm_port: 3000\n#    host_port: 3000\n#  - vm_port: 5432          # host_port defaults to 5432\n")
	}

	b.WriteString("\n# Optional. Extra directory shares. The project directory itself is ALWAYS\n")
	b.WriteString("# shared, read-write by default, and appears in a macOS guest at\n")
	b.WriteString("# " + GuestProjectPath() + " — list the reserved name \"project\"\n")
	b.WriteString("# below to change only its mode. Every other mount needs a host_path:\n")
	b.WriteString("# ~ expands to your home directory, relative paths resolve against this\n")
	b.WriteString("# project directory, and the path must exist before the VM starts.\n")
	if len(check.Mounts) > 0 {
		b.WriteString("mounts:\n")
		annotated := false // the mode legend earns its keep once, not per entry
		for _, m := range check.Mounts {
			b.WriteString("  - name: " + yamlScalar(m.Name) + "\n")
			if m.HostPath != "" {
				b.WriteString("    host_path: " + yamlScalar(m.HostPath) + "\n")
			}
			if m.Mode != "" {
				b.WriteString("    mode: " + yamlScalar(string(m.Mode)))
				if !annotated {
					b.WriteString("               # rw (default) | ro")
					annotated = true
				}
				b.WriteString("\n")
			}
		}
	} else {
		b.WriteString("#mounts:\n")
		b.WriteString("#  - name: project          # reserved: overrides the implicit project mount\n")
		b.WriteString("#    mode: ro               # rw (default) | ro\n")
		b.WriteString("#  - name: datasets\n")
		b.WriteString("#    host_path: ~/datasets\n")
		b.WriteString("#    mode: ro\n")
	}
	return []byte(b.String()), nil
}

// yamlScalar renders s the way the YAML encoder would, so that values needing
// quotes (a leading ~, a trailing colon, anything that would parse as a
// number or a null) survive the round trip through Load.
func yamlScalar(s string) string {
	out, err := yaml.Marshal(s)
	if err != nil { // yaml.Marshal of a string cannot fail
		return s
	}
	return strings.TrimRight(string(out), "\n")
}
