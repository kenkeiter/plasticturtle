package tart

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/kenkeiter/plasticturtle/internal/sys"
)

var _ Client = (*cli)(nil)

// cli is the exec-backed Client. It holds no state beyond the binary path and
// the runner: every method is a pure argv translation, so the supervisor's
// lifecycle logic has nowhere to hide down here.
type cli struct {
	bin string
	r   sys.Runner
}

// NewCLI returns a Client that shells out to the tart binary at bin (empty
// means DefaultBinary) via r.
func NewCLI(bin string, r sys.Runner) Client {
	if bin == "" {
		bin = DefaultBinary
	}
	return &cli{bin: bin, r: r}
}

func (c *cli) Clone(ctx context.Context, image, name string) error {
	_, err := c.r.Run(ctx, c.bin, "clone", image, name)
	// The failing name here is the source image, not the clone: tart reports
	// "does not exist" for a source it cannot resolve locally or in a registry.
	return mapErr(image, err)
}

func (c *cli) Set(ctx context.Context, name string, cpu, memoryMiB int) error {
	// Zero means "inherit from the image". With nothing to override there is
	// nothing to say to tart, and running `tart set <name>` with no flags would
	// still fail on a nonexistent VM — a failure the caller never asked for.
	if cpu == 0 && memoryMiB == 0 {
		return nil
	}
	args := []string{"set", name}
	if cpu != 0 {
		args = append(args, "--cpu", strconv.Itoa(cpu))
	}
	if memoryMiB != 0 {
		args = append(args, "--memory", strconv.Itoa(memoryMiB))
	}
	_, err := c.r.Run(ctx, c.bin, args...)
	return mapErr(name, err)
}

func (c *cli) Run(ctx context.Context, name string, opts RunOpts) (sys.Process, error) {
	args := []string{"run"}
	if opts.NoGraphics {
		args = append(args, "--no-graphics")
	}
	if opts.Softnet {
		args = append(args, "--net-softnet")
	}
	for _, d := range opts.Dirs {
		args = append(args, "--dir="+dirSpec(d))
	}
	args = append(args, name)

	// Start, not Run: `tart run` is the VM's lifetime. The handle is the only
	// way the supervisor learns that the guest died on its own.
	p, err := c.r.StartEnv(ctx, c.bin, opts.Env, args...)
	if err != nil {
		return nil, mapErr(name, err)
	}
	return p, nil
}

func (c *cli) Stop(ctx context.Context, name string, force bool) error {
	args := []string{"stop", name}
	if force {
		// tart 2.x has no --force flag; --timeout is the seconds it waits for a
		// graceful shutdown before killing the VM, so zero is "kill it now".
		// Omitting the flag leaves tart's own 30 s default, which is what the
		// design document asks for on the graceful attempt.
		args = append(args, "--timeout", "0")
	}
	_, err := c.r.Run(ctx, c.bin, args...)
	return mapErr(name, err)
}

func (c *cli) Delete(ctx context.Context, name string) error {
	_, err := c.r.Run(ctx, c.bin, "delete", name)
	return mapErr(name, err)
}

func (c *cli) IP(ctx context.Context, name string) (string, error) {
	out, err := c.r.Run(ctx, c.bin, "ip", name)
	if err != nil {
		return "", mapErr(name, err)
	}
	ip := strings.TrimSpace(string(out))
	if ip == "" {
		// Defensive: every tart version seen so far exits non-zero when it has
		// no lease, but an empty address is the same fact and callers polling
		// through boot must not mistake it for success.
		return "", fmt.Errorf("%s: %w", name, ErrNoIP)
	}
	return ip, nil
}

func (c *cli) List(ctx context.Context) ([]VM, error) {
	out, err := c.r.Run(ctx, c.bin, "list", "--format", "json")
	if err != nil {
		return nil, fmt.Errorf("tart list: %w", err)
	}
	return parseList(out)
}

// parseList decodes `tart list --format json`. tart reports the source of an
// OCI image as "OCI" but the local one as "local", so case is normalized here
// rather than forcing every caller to compare case-insensitively.
func parseList(out []byte) ([]VM, error) {
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return nil, nil
	}
	var vms []VM
	if err := json.Unmarshal([]byte(trimmed), &vms); err != nil {
		return nil, fmt.Errorf("tart list: parse json: %w", err)
	}
	for i := range vms {
		vms[i].Source = Source(strings.ToLower(string(vms[i].Source)))
		vms[i].State = State(strings.ToLower(string(vms[i].State)))
	}
	return vms, nil
}

// dirSpec renders one --dir value. An empty share name is legal and means
// "mount at the default tag with the host directory's own basename".
func dirSpec(d DirShare) string {
	spec := d.HostPath
	if d.Name != "" {
		spec = d.Name + ":" + d.HostPath
	}
	if d.ReadOnly {
		spec += ":ro"
	}
	return spec
}

// Substrings of tart's stderr that carry meaning for lifecycle decisions.
// Matching on prose is fragile, but tart exits 2 for both a missing VM and a
// dozen unrelated failures, so the exit code alone cannot distinguish them.
const (
	stderrNotFound = "does not exist"      // `the specified VM "x" does not exist`
	stderrNoIP     = "no ip address"       // `no IP address found, is your VM running?`
	stderrNoBinary = "file not found in $" // os/exec's "executable file not found in $PATH"
)

// mapErr promotes the two failures pt reacts to into sentinels and passes
// everything else through untouched. Anything unrecognized must stay an opaque
// error: silently classifying an unknown failure as "VM missing" would make a
// best-effort delete swallow a real problem.
func mapErr(name string, err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, stderrNoBinary):
		// "executable file not found in $PATH" is tart being absent, not the VM.
		return err
	case strings.Contains(msg, stderrNotFound):
		return fmt.Errorf("%s: %w", name, ErrNotFound)
	case strings.Contains(msg, stderrNoIP):
		return fmt.Errorf("%s: %w", name, ErrNoIP)
	}
	return err
}
