package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Process liveness.
//
// A bare kill(pid, 0) is not enough. State files survive reboots, and PIDs are
// reused: after a crash and restart, some unrelated process can inherit a dead
// supervisor's PID, at which point pt would conclude a VM is running and
// refuse to recreate it. Every recorded PID is therefore paired with that
// process's start time, read from the kernel via KERN_PROC_PID, and liveness
// means both still match.

// sZomb is BSD's SZOMB: the process has exited and is waiting to be reaped. Its
// PID and start time still match, but it can no longer be holding a pt session
// or supervising a VM, so pt counts it as dead. x/sys/unix does not export the
// p_stat constants.
const sZomb = 5

// procCommandTimeout bounds the helper subprocesses below. It is not a
// protocol timeout — nothing in the design's timing depends on its value, so
// it does not belong in ptcfg. It exists only so that a wedged filesystem
// cannot hang `plasticturtle list` indefinitely.
const procCommandTimeout = 10 * time.Second

// ProcStart returns the process start time for pid, as microseconds since the
// epoch, read from the kernel process table. It returns an error if the
// process does not exist.
func ProcStart(pid int) (uint64, error) {
	if pid <= 0 {
		// PID 0 is the kernel task and negative values are process groups;
		// KERN_PROC_PID answers for the former, which is never what a caller
		// checking a recorded PID means.
		return 0, fmt.Errorf("state: invalid pid %d", pid)
	}
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		// The kernel returns a short read for an unknown PID, which x/sys
		// surfaces as EIO rather than ESRCH; either way the process is gone.
		return 0, fmt.Errorf("state: kern.proc.pid %d: %w", pid, err)
	}
	if kp.Proc.P_stat == sZomb {
		return 0, fmt.Errorf("state: pid %d is a zombie", pid)
	}
	tv := kp.Proc.P_starttime
	return uint64(tv.Sec)*1_000_000 + uint64(tv.Usec), nil
}

// Self returns this process's PID and start time, for recording in a session
// or instance record.
func Self() (pid int, start uint64, err error) {
	pid = os.Getpid()
	start, err = ProcStart(pid)
	if err != nil {
		return pid, 0, err
	}
	return pid, start, nil
}

// Alive reports whether pid is running and was started at start. A zero start
// is treated as unverifiable and falls back to an existence check only —
// accepted for records written by older builds, never for new ones.
func Alive(pid int, start uint64) bool {
	if pid <= 0 {
		return false
	}
	actual, err := ProcStart(pid)
	if err != nil {
		return false
	}
	if start == 0 {
		return true
	}
	// The whole point: same PID, different birth, therefore a different
	// process that merely inherited the number.
	return actual == start
}

// ProcStats is resource usage for a process tree, used by plasticturtle list.
type ProcStats struct {
	CPUPercent float64
	RSSBytes   uint64
}

// TreeStats sums CPU and RSS across pid and its descendants.
//
// The interesting number for plasticturtle list is the cost of the whole `tart run`
// subtree, not of the launcher process, so this walks the parent links from a
// single ps snapshot rather than sampling each process separately.
func TreeStats(pid int) (ProcStats, error) {
	if pid <= 0 {
		return ProcStats{}, fmt.Errorf("state: invalid pid %d", pid)
	}
	out, err := runCommand("ps", "-Ao", "pid=,ppid=,pcpu=,rss=")
	if err != nil {
		return ProcStats{}, err
	}

	type row struct {
		cpu float64
		rss uint64
	}
	rows := make(map[int]row)
	children := make(map[int][]int)

	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 4 {
			continue
		}
		p, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		ppid, _ := strconv.Atoi(f[1])
		cpu, _ := strconv.ParseFloat(f[2], 64)
		// ps reports RSS in kilobytes on darwin.
		rssKB, _ := strconv.ParseUint(f[3], 10, 64)
		rows[p] = row{cpu: cpu, rss: rssKB * 1024}
		children[ppid] = append(children[ppid], p)
	}

	if _, ok := rows[pid]; !ok {
		return ProcStats{}, fmt.Errorf("state: pid %d not found", pid)
	}

	var stats ProcStats
	seen := map[int]bool{}
	queue := []int{pid}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		if seen[p] {
			continue
		}
		seen[p] = true
		r := rows[p]
		stats.CPUPercent += r.cpu
		stats.RSSBytes += r.rss
		queue = append(queue, children[p]...)
	}
	return stats, nil
}

// DiskUsageBytes reports the on-disk size of a tart VM directory.
//
// The number is approximate: APFS clones share blocks with the source image,
// and du charges shared blocks to whichever path it walks first. plasticturtle list
// labels the column accordingly rather than pretending otherwise.
func DiskUsageBytes(vmDir string) (uint64, error) {
	if vmDir == "" {
		return 0, errors.New("state: empty vm directory")
	}
	if _, err := os.Stat(vmDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// The clone has not been made yet, or is already deleted. Zero is
			// the honest answer, not a failure worth breaking plasticturtle list over.
			return 0, nil
		}
		return 0, fmt.Errorf("state: stat %s: %w", vmDir, err)
	}
	out, err := runCommand("du", "-sk", vmDir)
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return 0, fmt.Errorf("state: du reported nothing for %s", vmDir)
	}
	kb, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("state: parse du output %q: %w", fields[0], err)
	}
	return kb * 1024, nil
}

// TartVMDir returns ~/.tart/vms/<name>.
func TartVMDir(name string) (string, error) {
	if name == "" {
		return "", errors.New("state: empty vm name")
	}
	// tart itself honors TART_HOME; following it keeps plasticturtle list's disk column
	// pointing at the same files tart is actually using.
	if home := os.Getenv("TART_HOME"); home != "" {
		return filepath.Join(home, "vms", name), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("state: locate home directory: %w", err)
	}
	return filepath.Join(home, ".tart", "vms", name), nil
}

// runCommand is the one place this package shells out. os/exec appears here
// and nowhere else in internal/state.
func runCommand(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), procCommandTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return nil, fmt.Errorf("state: %s %s: %w", name, strings.Join(args, " "), err)
	}
	return out, nil
}
