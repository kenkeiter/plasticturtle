package supervisor

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/kenkeiter/plasticturtle/internal/config"
	"github.com/kenkeiter/plasticturtle/internal/netfw"
)

// restricted reports whether this instance runs under the domain firewall.
func (r *run) restricted() bool {
	return r.p.Config != nil && r.p.Config.Network.Policy == config.NetRestricted
}

// networkRunOpts returns the tart run additions for the network policy: for an
// open policy, the zero values (unchanged behavior); for a restricted policy,
// --net-softnet plus the environment that shadows softnet with the firewall
// shim and names the written policy file.
//
// It fails the boot if the shim is not correctly installed. That is deliberate:
// a restricted policy that silently fell back to open networking would be a
// firewall that is not there, which is worse than not booting.
func (r *run) networkRunOpts() (softnet bool, env []string, err error) {
	if !r.restricted() {
		return false, nil, nil
	}
	shim := r.d.Store.ShimPath()
	if err := verifyShim(shim, os.Stat); err != nil {
		return false, nil, fmt.Errorf("restricted network policy needs the firewall shim: %w\n"+
			"install it once with: pt setup-firewall", err)
	}
	policyPath, err := r.writePolicyFile()
	if err != nil {
		return false, nil, err
	}

	shimDir := filepath.Dir(shim)
	env = []string{
		// Shim dir first so tart resolves our "softnet"; the rest of PATH stays
		// so tart itself (and the shim's trusted-path lookup fallbacks) resolve.
		"PATH=" + shimDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"PT_NETFW_POLICY=" + policyPath,
		"PT_SHIM_LOG=" + filepath.Join(r.d.Store.ProjectDir(r.p.ProjectID), "softnet-shim.log"),
	}
	r.logf("network: restricted; %d allowed domains; shim %s", len(r.p.Config.Network.Allow), shim)
	return true, env, nil
}

// writePolicyFile renders the resolved allowlist as the shim's policy document
// and writes it into the instance directory, returning its path.
func (r *run) writePolicyFile() (string, error) {
	doc := &netfw.PolicyDoc{
		Policy: string(config.NetRestricted),
		Allow:  r.p.Config.Network.Allow,
	}
	raw, err := doc.Marshal()
	if err != nil {
		return "", fmt.Errorf("network: marshal policy: %w", err)
	}
	path := filepath.Join(r.d.Store.ProjectDir(r.p.ProjectID), "netfw-policy.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", fmt.Errorf("network: write policy: %w", err)
	}
	return path, nil
}

// VerifyShim checks that the installed firewall shim is safe for tart to run
// setuid. It is exported so `pt setup-firewall` fails setup the same way a boot
// would, from one definition of "correctly installed".
func VerifyShim(path string) error { return verifyShim(path, os.Stat) }

// verifyShim checks that path is a firewall shim safe for tart to run setuid:
// a regular file owned by root, with the setuid bit set, and not writable by
// group or other. Any failure is actionable by reinstalling the shim.
func verifyShim(path string, stat func(string) (os.FileInfo, error)) error {
	fi, err := stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("shim not installed at %s", path)
		}
		return fmt.Errorf("stat shim %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("shim %s is not a regular file", path)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("shim %s: cannot read ownership", path)
	}
	if st.Uid != 0 {
		return fmt.Errorf("shim %s is not owned by root (uid %d); re-run setup", path, st.Uid)
	}
	if fi.Mode()&os.ModeSetuid == 0 {
		return fmt.Errorf("shim %s is missing its setuid bit; re-run setup", path)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("shim %s is group/world writable (%#o); re-run setup", path, fi.Mode().Perm())
	}
	return nil
}

// hostIface is a host network interface reduced to what collision detection
// needs. It is separated from net.Interface so the detector is unit-testable.
type hostIface struct {
	name     string
	prefixes []netip.Prefix
	isVMNet  bool // a vmnet bridge (bridge*), which legitimately shares the subnet
}

// checkVMNetCollision aborts the boot if the guest's subnet collides with a real
// host network. Under softnet, vmnet chooses the guest subnet; when that choice
// lands on the same range as the host's LAN, the host's bridge steals addresses
// (a gateway, a DNS server) from the real network and host connectivity breaks.
// Detecting it lets pt tear down — restoring the host — with a clear message,
// rather than leaving the user with mysteriously broken networking.
func (r *run) checkVMNetCollision() error {
	if !r.restricted() || r.vmIP == "" {
		return nil
	}
	guest, err := netip.ParseAddr(r.vmIP)
	if err != nil {
		return nil // not fatal; if we cannot parse it we cannot judge it
	}
	ifaces, err := readHostIfaces()
	if err != nil {
		r.logf("network: could not enumerate host interfaces for collision check: %v", err)
		return nil
	}
	if detail, collides := detectSubnetCollision(guest, ifaces); collides {
		return fmt.Errorf("network: the sandbox subnet collides with a host network (%s).\n"+
			"This breaks host connectivity. Pin vmnet to an unused range (see `pt setup-firewall`) and retry", detail)
	}
	return nil
}

// detectSubnetCollision reports whether guest's /24 overlaps the network of any
// non-vmnet host interface. The guest address itself lives on a vmnet bridge, so
// bridge interfaces are excluded; a hit on any other interface means the two
// networks share address space.
func detectSubnetCollision(guest netip.Addr, ifaces []hostIface) (string, bool) {
	guest24 := netip.PrefixFrom(guest, 24).Masked()
	for _, ifc := range ifaces {
		if ifc.isVMNet {
			continue
		}
		for _, p := range ifc.prefixes {
			if !p.Addr().Is4() {
				continue
			}
			// Collision if either network contains the other's base address.
			if guest24.Contains(p.Addr()) || p.Masked().Contains(guest) {
				return fmt.Sprintf("%s on interface %s overlaps sandbox %s", p, ifc.name, guest24), true
			}
		}
	}
	return "", false
}

// readHostIfaces snapshots the host's interfaces for collision detection.
func readHostIfaces() ([]hostIface, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []hostIface
	for _, ifc := range ifs {
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		h := hostIface{
			name:    ifc.Name,
			isVMNet: strings.HasPrefix(ifc.Name, "bridge"),
		}
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				if p, ok := netipPrefixFromIPNet(ipn); ok {
					h.prefixes = append(h.prefixes, p)
				}
			}
		}
		out = append(out, h)
	}
	return out, nil
}

func netipPrefixFromIPNet(n *net.IPNet) (netip.Prefix, bool) {
	addr, ok := netip.AddrFromSlice(n.IP)
	if !ok {
		return netip.Prefix{}, false
	}
	ones, _ := n.Mask.Size()
	return netip.PrefixFrom(addr.Unmap(), ones), true
}
