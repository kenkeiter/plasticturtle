// Package config parses and validates the .plasticturtle project file.
//
// This is a security-relevant file: it names the image a project boots, the
// host directories exposed to it, and the ports opened on the host. Decoding
// is therefore strict (unknown keys are errors) and validation is total —
// every field is checked before any of it reaches tart.
package config

import (
	"errors"
	"time"
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

// Find walks upward from startDir looking for FileName, stopping at the
// filesystem root. It returns the canonical absolute path of the directory
// containing the file, or ErrNotFound.
//
// Canonicalization happens here so that a project reached through a symlink is
// the same project as one reached directly (see spec section 10).
func Find(startDir string) (projectDir string, err error) {
	panic("TODO(wave1): config.Find")
}

// Load reads and strictly decodes the config in projectDir. It returns the
// parsed config alongside the exact bytes read — the bytes are what trust
// hashing operates on, so they must not be re-serialized.
//
// Load does not validate; call Validate.
func Load(projectDir string) (cfg *Config, raw []byte, err error) {
	panic("TODO(wave1): config.Load")
}

// Validate enforces every rule in spec section 3.1. It reports all violations
// it can find, joined, rather than only the first: a user fixing a config
// should not have to run pt allow five times.
func (c *Config) Validate() error {
	panic("TODO(wave1): config.Validate")
}

// Resolve applies defaults and expands paths against projectDir, which must be
// canonical and absolute. It returns an error if a host path does not exist —
// a missing mount source is a hard error before any VM is cloned.
//
// Resolve assumes Validate has already passed.
func (c *Config) Resolve(projectDir string) (*Resolved, error) {
	panic("TODO(wave1): config.Resolve")
}

// HashBytes returns the trust hash of a config file's exact bytes, formatted
// "sha256:<hex>".
func HashBytes(raw []byte) string {
	panic("TODO(wave1): config.HashBytes")
}

// GuestProjectPath is where the project share appears inside a macOS guest.
func GuestProjectPath() string { return GuestSharesRoot + "/" + ProjectMountName }

// Template renders a commented .plasticturtle for pt init. The comments are
// part of the deliverable: this file is meant to be human-edited later.
func Template(c *Config, generatedAt time.Time) ([]byte, error) {
	panic("TODO(wave3): config.Template")
}
