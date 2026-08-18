// Package tart is a thin wrapper over the tart CLI.
//
// It contains no policy: it translates Go calls into argv, runs them through a
// sys.Runner, and parses --format json where tart offers it. All lifecycle
// decisions live in the supervisor. Keeping this layer dumb is what lets the
// supervisor be tested against Fake without a hypervisor.
package tart

import (
	"context"
	"errors"

	"github.com/kenkeiter/plasticturtle/internal/sys"
)

// DefaultBinary is the tart executable, resolved from PATH.
const DefaultBinary = "tart"

// Sentinel errors, mapped from tart's exit codes and stderr.
var (
	// ErrNotFound means the named VM does not exist. Callers treat this as
	// success when deleting.
	ErrNotFound = errors.New("tart: vm not found")

	// ErrNoIP means the guest has not yet acquired a DHCP lease. It is the
	// expected error while polling during boot, not a failure.
	ErrNoIP = errors.New("tart: vm has no ip yet")
)

// Source is where a VM or image came from.
type Source string

const (
	SourceLocal Source = "local"
	SourceOCI   Source = "oci"
)

// State is a VM's run state as reported by tart list.
type State string

const (
	StateRunning State = "running"
	StateStopped State = "stopped"
)

// VM is one row of tart list.
type VM struct {
	Source Source `json:"Source"`
	Name   string `json:"Name"`
	DiskGB int    `json:"Disk"`
	SizeGB int    `json:"Size"`
	State  State  `json:"State"`
}

// DirShare is one --dir=name:path[:ro] argument.
type DirShare struct {
	Name     string
	HostPath string
	ReadOnly bool
}

// RunOpts configures tart run.
type RunOpts struct {
	// NoGraphics is always true for pt; the field exists so the flag is
	// explicit at the call site rather than buried in the wrapper.
	NoGraphics bool
	Dirs       []DirShare
}

// Client is the tart surface pt uses. Every method takes a context so boot
// polling and teardown can be bounded.
type Client interface {
	// Clone creates a CoW clone of image (a local name or OCI reference) as
	// name.
	Clone(ctx context.Context, image, name string) error

	// Set overrides the clone's resources. Zero values mean "leave alone", so
	// callers pass exactly what the config overrode.
	Set(ctx context.Context, name string, cpu, memoryMiB int) error

	// Run boots the VM as a child process and returns its handle. The caller
	// owns the handle and must Wait on it.
	Run(ctx context.Context, name string, opts RunOpts) (sys.Process, error)

	// Stop stops the VM, forcibly if force is set.
	Stop(ctx context.Context, name string, force bool) error

	// Delete removes the clone from disk. Deleting a nonexistent VM returns
	// ErrNotFound, which callers doing best-effort cleanup should ignore.
	Delete(ctx context.Context, name string) error

	// IP returns the guest's address, or ErrNoIP if it has none yet.
	IP(ctx context.Context, name string) (string, error)

	// List returns all VMs and images known to tart, local and OCI-cached.
	List(ctx context.Context) ([]VM, error)
}

// The CLI implementation lives in cli.go and the in-memory Fake in fake.go;
// this file is the frozen surface both of them satisfy.
