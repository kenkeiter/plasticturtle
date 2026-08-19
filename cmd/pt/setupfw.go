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

	// The shim shadows softnet on tart's PATH, so tart no longer roots the real
	// softnet the way plain `tart --net-softnet` would. We must do it here: the
	// shim execs the real softnet as root and refuses to unless it is root-owned
	// and unwritable. Root both binaries in one escalation.
	realSoftnet, err := locateRealSoftnet()
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Found Softnet: %s\n", realSoftnet)

	// chown before chmod: chown clears the setuid bit, so setting it last is what
	// makes the order load-bearing. One sudo invocation does all of it.
	fmt.Fprintln(out, "Granting root to the shim and Softnet (sudo may prompt for your password)…")
	script := fmt.Sprintf("chown root %q %q && chmod u+s %q %q", dst, realSoftnet, dst, realSoftnet)
	if err := priv("sh", "-c", script); err != nil {
		return fmt.Errorf("make shim/softnet setuid-root: %w", err)
	}

	if err := verifyShim(dst); err != nil {
		return fmt.Errorf("shim did not install correctly: %w", err)
	}
	fmt.Fprintln(out, "Firewall shim is installed and setuid-root.")
	fmt.Fprintln(out, "Projects with `network: { policy: restricted }` will now be enforced.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Note: software networking assigns the sandbox a private subnet. If it")
	fmt.Fprintln(out, "collides with your LAN, pt will refuse the boot and tell you. You can pin")
	fmt.Fprintln(out, "vmnet to an unused range in /etc/bootpd.plist or com.apple.vmnet.plist.")
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

// trustedSoftnetPaths mirrors the shim's own list: the standard Homebrew
// locations Softnet ships to. setup roots whichever it finds so the shim's
// safety check will later pass.
var trustedSoftnetPaths = []string{
	"/opt/homebrew/bin/softnet",
	"/usr/local/bin/softnet",
}

// locateRealSoftnet returns the resolved path of the installed Softnet binary,
// following symlinks (Homebrew's bin/softnet points into Cellar) so the chown
// targets the real file rather than a link.
func locateRealSoftnet() (string, error) {
	for _, p := range trustedSoftnetPaths {
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			continue
		}
		if fi, err := os.Stat(real); err == nil && fi.Mode().IsRegular() {
			return real, nil
		}
	}
	return "", fmt.Errorf("Softnet is not installed in %v; install it (e.g. `brew install cirruslabs/cli/softnet`) and re-run", trustedSoftnetPaths)
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
