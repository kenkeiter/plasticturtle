package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/kenkeiter/plasticturtle/internal/state"
	"github.com/kenkeiter/plasticturtle/internal/supervisor"
)

// privRunner runs a privileged command (via sudo). It is a seam so the install
// logic can be tested without actually escalating.
type privRunner func(argv ...string) error

// sudoRunner runs argv under sudo with the terminal attached, so its password
// prompt reaches the user — the same interactive escalation tart uses to set the
// softnet SUID bit.
func sudoRunner(argv ...string) error {
	cmd := exec.Command("sudo", argv...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stderr // keep stdout clean for any structured output
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// runSetupFirewall installs the firewall shim so restricted network policies can
// be enforced. It copies the shim next to pt into the state tree and makes it
// setuid-root, then verifies the result.
func runSetupFirewall(store *state.Store, shimSrc string, out io.Writer, priv privRunner) error {
	src, err := locateShimBinary(shimSrc)
	if err != nil {
		return err
	}
	dstDir := store.FirewallDir()
	dst := store.ShimPath()
	if err := os.MkdirAll(dstDir, 0o700); err != nil {
		return fmt.Errorf("create firewall dir: %w", err)
	}
	if err := copyFile(src, dst, 0o755); err != nil {
		return fmt.Errorf("install shim: %w", err)
	}
	fmt.Fprintf(out, "Installed firewall shim: %s\n", dst)

	// The shim shadows softnet on tart's PATH, so tart no longer roots it the way
	// plain `tart --net-softnet` would. We must do it here: the shim creates the
	// VM's vmnet interface, which needs root.
	//
	// chown before chmod: chown clears the setuid bit, so setting it last is what
	// makes the order load-bearing. One sudo invocation does all of it.
	fmt.Fprintln(out, "Granting root to the shim (sudo may prompt for your password)…")
	script := fmt.Sprintf("chown root %q && chmod u+s %q", dst, dst)
	if err := priv("sh", "-c", script); err != nil {
		return fmt.Errorf("make shim setuid-root: %w", err)
	}

	if err := verifyShim(dst); err != nil {
		return fmt.Errorf("shim did not install correctly: %w", err)
	}
	fmt.Fprintln(out, "Firewall shim is installed and setuid-root.")
	fmt.Fprintln(out, "Projects with `network: { policy: restricted }` will now be enforced.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Note: the shim provides the sandbox's networking itself. pt picks the")
	fmt.Fprintln(out, "sandbox subnet from 192.168.200.0/24–192.168.252.0/24, skipping any range")
	fmt.Fprintln(out, "your host already uses, and refuses the boot if one collides anyway.")
	return nil
}

// verifyShim here mirrors supervisor.VerifyShim so setup fails the same way a
// boot would. It is exported from the supervisor package for exactly this reuse.
func verifyShim(path string) error { return supervisor.VerifyShim(path) }

// locateShimBinary finds the shim to install: an explicit path wins, then
// PT_SHIM_BIN, then a binary named pt-softnet-shim next to the running pt.
func locateShimBinary(explicit string) (string, error) {
	candidates := []string{}
	if explicit != "" {
		candidates = append(candidates, explicit)
	}
	if env := os.Getenv("PT_SHIM_BIN"); env != "" {
		candidates = append(candidates, env)
	}
	if self, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(self), "pt-softnet-shim"))
	}
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && fi.Mode().IsRegular() {
			return c, nil
		}
	}
	return "", errors.New("could not find the pt-softnet-shim binary; build it with `make build` " +
		"and keep it next to pt, or pass --shim <path>")
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	// Replace atomically-ish: write a temp then rename, so a half-copied shim is
	// never left where tart might find it.
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}
