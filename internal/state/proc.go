package state

// Process liveness.
//
// A bare kill(pid, 0) is not enough. State files survive reboots, and PIDs are
// reused: after a crash and restart, some unrelated process can inherit a dead
// supervisor's PID, at which point pt would conclude a VM is running and
// refuse to recreate it. Every recorded PID is therefore paired with that
// process's start time, read from the kernel via KERN_PROC_PID, and liveness
// means both still match.

// ProcStart returns the process start time for pid, as microseconds since the
// epoch, read from the kernel process table. It returns an error if the
// process does not exist.
func ProcStart(pid int) (uint64, error) { panic("TODO(wave1): state.ProcStart") }

// Self returns this process's PID and start time, for recording in a session
// or instance record.
func Self() (pid int, start uint64, err error) { panic("TODO(wave1): state.Self") }

// Alive reports whether pid is running and was started at start. A zero start
// is treated as unverifiable and falls back to an existence check only —
// accepted for records written by older builds, never for new ones.
func Alive(pid int, start uint64) bool { panic("TODO(wave1): state.Alive") }

// ProcStats is resource usage for a process tree, used by pt list.
type ProcStats struct {
	CPUPercent float64
	RSSBytes   uint64
}

// TreeStats sums CPU and RSS across pid and its descendants.
func TreeStats(pid int) (ProcStats, error) { panic("TODO(wave3): state.TreeStats") }

// DiskUsageBytes reports the on-disk size of a tart VM directory.
//
// The number is approximate: APFS clones share blocks with the source image,
// and du charges shared blocks to whichever path it walks first. pt list
// labels the column accordingly rather than pretending otherwise.
func DiskUsageBytes(vmDir string) (uint64, error) { panic("TODO(wave3): state.DiskUsageBytes") }

// TartVMDir returns ~/.tart/vms/<name>.
func TartVMDir(name string) (string, error) { panic("TODO(wave3): state.TartVMDir") }
