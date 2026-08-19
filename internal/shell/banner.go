package shell

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/kenkeiter/plasticturtle/internal/ptcfg"
	"github.com/kenkeiter/plasticturtle/internal/sshx"
	"github.com/kenkeiter/plasticturtle/internal/state"
	"github.com/mattn/go-runewidth"
)

// Banner colors, xterm-256. The dark green background is what makes the row
// read as chrome rather than guest output; white and palegreen1 are the only
// two foregrounds on it.
const (
	bannerBG    = "\x1b[48;5;22m"  // dark green
	bannerWhite = "\x1b[38;5;15m"  // white
	bannerGreen = "\x1b[38;5;121m" // palegreen1
)

// bannerNameMax caps the image-name segment so one long OCI reference cannot
// eat the whole row.
const bannerNameMax = 24

// bannerStats are the figures the poll loop feeds the renderer. Comparable on
// purpose: "did anything change" is one struct compare.
type bannerStats struct {
	shells    int
	cpuPct    float64
	memGB     float64
	haveUsage bool
}

// banner is the status line pt shell shows while a session is attached: the
// sandbox identity on the left, the instance's live figures on the right.
type banner struct {
	image   string // the base image the sandbox was cloned from
	warnNet bool   // network policy is open: say so, loudly
	persist bool   // this VM *is* the base image: say that too
	line    *sshx.StatusLine

	// usage samples the VM's host-side CPU and memory; nil means the figures
	// are unavailable and the banner shows the shell count alone.
	usage func(context.Context) (cpuPct, memGB float64, err error)

	mu    sync.Mutex
	stats bannerStats
}

// newBanner takes both names because they serve different masters: image is
// what the row displays (the instance name is a generated handle nobody
// recognizes), while vmName is what locates the VM's disk for the usage
// sampler. Under --persist they are the same name arriving by two routes.
func newBanner(image string, warnNet bool, vmName string, vmPID int, persist bool) *banner {
	b := &banner{image: image, warnNet: warnNet, persist: persist}
	b.usage = newUsageSampler(vmName, vmPID).sample
	b.line = &sshx.StatusLine{Render: b.render}
	return b
}

// render produces the styled row, at most width cells wide. It runs under the
// status bar's lock (see sshx.StatusLine), so it only formats — the slow work
// happened in the poll loop.
//
// Degradation order when the terminal narrows: the right-hand figures go
// first, then the network warning, then the name; the turtle is last to go.
func (b *banner) render(width int) string {
	b.mu.Lock()
	st := b.stats
	b.mu.Unlock()

	name := truncateTail(b.image, bannerNameMax)
	warn := ""
	if b.warnNet {
		warn = " ⚠️ unrestricted network"
	}

	// The word carries the one fact a user cannot recover by looking around:
	// whether what they type here survives the session. It replaces "Sandbox"
	// rather than joining it so that the row's width behavior — and every
	// narrowing step below — is unchanged.
	label := "Sandbox"
	if b.persist {
		label = "Persistent"
	}

	// Each candidate is (plain text for width accounting, styled equivalent).
	// The styled string must render exactly the plain one's cells.
	left := func(name, warn string) (string, string) {
		plain := " 🐢 " + label + " [" + name + "]" + warn + " "
		styled := bannerWhite + " 🐢 " + label + " " + bannerGreen + "[" + name + "]" + bannerWhite + warn + " "
		return plain, styled
	}

	right := b.rightText(st)

	// The row is painted onto a background-colored ESC[2K erase, so a render
	// narrower than the terminal still leaves a fully green row; only content
	// wider than the terminal is dangerous (it wraps into the guest's rows).
	plain, styled := left(name, warn)
	switch lw, rw := cellWidth(plain), cellWidth(right); {
	case right != "" && lw+rw+1 <= width:
		pad := strings.Repeat(" ", width-lw-rw)
		return bannerBG + "\x1b[2K" + styled + pad + bannerGreen + right
	case lw <= width:
		return bannerBG + "\x1b[2K" + styled
	}
	if plain, styled = left(name, ""); cellWidth(plain) <= width {
		return bannerBG + "\x1b[2K" + styled
	}
	if plain = " 🐢 [" + name + "] "; cellWidth(plain) <= width {
		return bannerBG + "\x1b[2K" + bannerWhite + " 🐢 " + bannerGreen + "[" + name + "] "
	}
	if cellWidth(" 🐢 ") <= width {
		return bannerBG + "\x1b[2K" + " 🐢 "
	}
	return bannerBG + "\x1b[2K"
}

// rightText formats the live figures, without color: the caller styles it.
// The trailing space keeps the last glyph off the terminal's final column,
// where a pending-wrap miscount would be one cell away.
func (b *banner) rightText(st bannerStats) string {
	if st.shells <= 0 {
		return ""
	}
	noun := "shells"
	if st.shells == 1 {
		noun = "shell"
	}
	if !st.haveUsage {
		return fmt.Sprintf("%d %s ", st.shells, noun)
	}
	return fmt.Sprintf("%d %s → %.0f%% CPU / %.1fGB MEM ", st.shells, noun, st.cpuPct, st.memGB)
}

// truncateTail fits s into max cells by dropping the front, because an image
// name's tail is the part that identifies it: "…/macos-tahoe-base:latest"
// beats "ghcr.io/cirruslabs/mac…".
func truncateTail(s string, max int) string {
	if cellWidth(s) <= max {
		return s
	}
	runes := []rune(s)
	for i := 1; i < len(runes); i++ {
		if tail := "…" + string(runes[i:]); cellWidth(tail) <= max {
			return tail
		}
	}
	return "…"
}

// cellWidth counts terminal columns, counting East-Asian-ambiguous runes
// (this banner's ⚠ and →) as two. Terminals genuinely disagree about those,
// and the two failure modes are not symmetric: a row computed too narrow
// leaves a background-colored sliver at the right edge, while a row computed
// too wide wraps the cursor into the guest's rows and corrupts the display.
// Overestimating buys the harmless one.
func cellWidth(s string) int {
	w := 0
	prev := 0 // columns booked for the previous rune
	for _, r := range s {
		switch r {
		case 0xFE0F: // VS16: the previous rune renders in emoji style, two columns
			w += 2 - prev
			prev = 2
			continue
		case 0xFE0E: // VS15: text style, no extra columns
			continue
		case '…':
			// Ambiguous like ⚠ and →, but exempt from the doubling below: it
			// appears at most once per row (the truncation marker), so if a
			// terminal does render it two cells wide the row's final glyph
			// lands exactly in the last column — a deferred wrap the cursor
			// restore cancels — rather than wrapping. Counting it as 2 would
			// instead shave a real character off every truncated name.
			w++
			prev = 1
			continue
		}
		rw := runewidth.RuneWidth(r)
		if rw == 1 && runewidth.IsAmbiguousWidth(r) {
			rw = 2
		}
		w += rw
		prev = rw
	}
	return w
}

// poll keeps the banner's figures fresh until ctx is cancelled. One loop per
// attached shell: each samples on its own clock and repaints only on change,
// so an idle VM costs one ps(1) and one shared-lock directory read per tick.
func (b *banner) poll(ctx context.Context, d Deps, projectID string) {
	tk := d.Clock.NewTicker(ptcfg.BannerRefreshInterval)
	defer tk.Stop()
	for {
		b.sample(ctx, d, projectID)
		select {
		case <-tk.C():
		case <-ctx.Done():
			return
		}
	}
}

func (b *banner) sample(ctx context.Context, d Deps, projectID string) {
	st := bannerStats{shells: countLiveSessions(d.Store, projectID)}
	if b.usage != nil {
		if cpu, mem, err := b.usage(ctx); err == nil {
			st.cpuPct, st.memGB, st.haveUsage = cpu, mem, true
		}
	}

	b.mu.Lock()
	changed := st != b.stats
	b.stats = st
	b.mu.Unlock()
	if changed {
		b.line.Refresh()
	}
}

// countLiveSessions counts attached shells the way pt list would: records
// whose process is still alive. It reads under the shared lock and never
// deletes — reaping stale records is the supervisor's job, not a banner's.
// Every failure mode reports 1: this shell is attached, or it would not be
// asking.
func countLiveSessions(store *state.Store, projectID string) int {
	lk, err := store.RLock(projectID)
	if err != nil {
		return 1
	}
	defer func() { _ = lk.Unlock() }()

	sessions, err := store.ListSessions(projectID)
	if err != nil {
		return 1
	}
	n := 0
	for _, s := range sessions {
		if state.Alive(s.PID, s.ProcStart) {
			n++
		}
	}
	if n == 0 {
		n = 1
	}
	return n
}

// usageSampler finds and samples the host processes that carry the VM.
//
// The `tart run` child recorded in the instance record is NOT where the VM
// lives on modern macOS: Virtualization.framework hosts the guest's vCPUs and
// memory in a com.apple.Virtualization.VirtualMachine XPC helper, and that
// helper is launchd's child, not tart's — sampling tart alone reads a
// near-idle 45MB shim while the real guest burns cores in a process the tree
// walk can never reach. What does identify the helper is the instance's disk
// image, which it holds open; lsof on that one path names it.
//
// lsof costs ~200ms, so the resolved set is cached for the session and
// rebuilt only when every process in it has disappeared. ps carries each
// tick: one invocation for the whole set, summed.
type usageSampler struct {
	tartPID int
	disk    string
	pids    []int // resolved sample set; empty means "resolve again"
}

func newUsageSampler(instanceName string, tartPID int) *usageSampler {
	return &usageSampler{
		tartPID: tartPID,
		disk:    filepath.Join(tartHome(), "vms", instanceName, "disk.img"),
	}
}

// sample returns the summed CPU percentage (may exceed 100 on a multi-core
// VM) and resident memory in GB across the VM's processes. It is called from
// the banner's poll goroutine only, so the cached set needs no lock.
func (u *usageSampler) sample(ctx context.Context) (cpuPct, memGB float64, err error) {
	if len(u.pids) == 0 {
		u.pids = u.resolve(ctx)
	}
	if len(u.pids) == 0 {
		return 0, 0, errors.New("banner: no vm process found")
	}
	cpu, rssKB, rows, err := psUsage(ctx, u.pids)
	if err != nil || rows == 0 {
		// Nothing in the set answered: the processes are gone, or the set
		// was resolved against a world that has moved. Reporting stale
		// figures for the wrong process would be worse than a blank, so
		// rebuild on the next tick.
		u.pids = nil
		if err == nil {
			err = errors.New("banner: vm processes disappeared")
		}
		return 0, 0, err
	}
	return cpu, rssKB / (1024 * 1024), nil
}

// resolve names the VM's processes: whoever holds the instance's disk image
// open (the Virtualization helper, and usually tart itself), plus the
// recorded tart run PID as both a fallback for platforms that run the VM
// in-process and the only answer when lsof finds nothing.
func (u *usageSampler) resolve(ctx context.Context) []int {
	pids := lsofOpenPIDs(ctx, u.disk)
	if u.tartPID > 0 && !slices.Contains(pids, u.tartPID) {
		pids = append(pids, u.tartPID)
	}
	return pids
}

// tartHome mirrors tart's own data-directory rule: TART_HOME, else ~/.tart.
func tartHome() string {
	if h := os.Getenv("TART_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".tart")
}

// lsofOpenPIDs returns the PIDs holding path open. A package variable so
// tests can substitute process sets without a live hypervisor. lsof exits
// non-zero when nobody has the file open, which is an empty answer here, not
// an error.
var lsofOpenPIDs = func(ctx context.Context, path string) []int {
	out, _ := exec.CommandContext(ctx, "lsof", "-t", path).Output()
	var pids []int
	for _, f := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(f); err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids
}

// psUsage samples the given processes in one ps invocation, returning summed
// CPU, summed RSS in KB, and how many of them ps still knows. ps exits
// non-zero when any requested PID is gone but still reports the live ones,
// so its output is parsed regardless and the error is kept only when nothing
// came back. A package variable for the same reason as lsofOpenPIDs.
var psUsage = func(ctx context.Context, pids []int) (cpuPct, rssKB float64, rows int, err error) {
	strs := make([]string, len(pids))
	for i, pid := range pids {
		strs[i] = strconv.Itoa(pid)
	}
	out, runErr := exec.CommandContext(ctx, "ps", "-o", "%cpu=,rss=", "-p", strings.Join(strs, ",")).Output()

	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		cpu, cerr := strconv.ParseFloat(fields[0], 64)
		rss, rerr := strconv.ParseFloat(fields[1], 64)
		if cerr != nil || rerr != nil {
			continue
		}
		cpuPct += cpu
		rssKB += rss
		rows++
	}
	if rows == 0 && runErr != nil {
		return 0, 0, 0, fmt.Errorf("banner: sample vm usage: %w", runErr)
	}
	return cpuPct, rssKB, rows, nil
}
