# Plastic Turtle — Implementation Plan

**Companion to:** [`spec.md`](./spec.md)
**Execution model:** parallel Opus 5 subagents working in one shared tree, with disjoint
file ownership per agent and a compile-gated wave structure.
**Environment (verified):** Go 1.26.6 darwin/arm64, `tart` at `/opt/homebrew/bin/tart`,
repo currently contains only `doc/spec.md` and is **not** a git repo.

---

## 0. Prerequisites — **done**

1. ~~`git init`~~ — done (branch `main`, no commits yet, no remote).
2. `go mod init github.com/kenkeiter/plasticturtle` — wave 0.
3. ~~Tart image for the wave-5 smoke test~~ — done:
   `ghcr.io/cirruslabs/macos-tahoe-base:latest` (also present as local image `tahoe-base`).
   **This is the image used in docs, `pt init` examples, and the wave-5 integration test** —
   the spec's `macos-sequoia-base` references are illustrative only.

---

## 1. Spec decisions to lock before coding

These are gaps or internal tensions in `spec.md`. Each has a recommended resolution; the
wave-0 agent bakes the resolution into the interface skeletons so no wave-1 agent has to
guess. Overrule any of these before kickoff — they are cheap now and expensive in wave 3.

| # | Issue | Resolution |
|---|---|---|
| 1 | §6.2 shows the supervisor establishing tunnels, but §8.1.4 requires the *shell* to negotiate ports before spawning it. | Ordering is: shell resolves config → validates mount host paths exist → negotiates/binds host ports interactively → releases probe listeners → writes `instance.json{state:creating, ports:[resolved]}` → spawns supervisor with the resolved port list on **stdin as JSON** (not argv; keeps paths and ports out of `ps` output). Supervisor never prompts. |
| 2 | §7 says mount host paths are validated "at instance start, before cloning" — but cloning happens in the detached supervisor, where errors are invisible. | Validate in `pt shell` **before** spawning the supervisor, and again in the supervisor (defense in depth). User-facing errors must originate from the interactive process. |
| 3 | §8 says the tunnel pipes to `vmIp:<vmPort>` *over the SSH connection*. Dialing the VM's own external IP from inside the VM is wrong for services bound to loopback. | Remote dial target is `127.0.0.1:<vmPort>`; on dial failure fall back once to `vmIp:<vmPort>` and log which one succeeded. |
| 4 | §3.2's PID-reuse guard is specified loosely ("compare start time via sysctl/ps"). | Persist `{pid, startTicks}` in every record. Liveness = `kill(pid,0)` succeeds **and** `KERN_PROC_PID` start time matches. Implement once in `internal/state` (`proc.go`); nothing else may re-implement it. |
| 5 | `pt zsh-hook` and `pt _check-trust` appear in §5.1 but are absent from the §4 CLI surface. | Both are real hidden subcommands; wave 0 registers them. |
| 6 | §4.5 `DISK` via `du -sk`. On APFS, CoW clones share blocks and `du` charges them to whichever path it walks first. | Use `du -sk` as spec'd, but label the column `DISK*` and footnote "approximate; CoW clones share blocks with the source image." Do not invent a smarter accounting scheme in v1. |
| 7 | §7 login preamble "exports `PT_IN_VM_SESSION=1`". `ssh.Session.Setenv` is refused by default `sshd` (`AcceptEnv`). | Do not use `Setenv`. Request PTY, then run the command string: `cd '/Volumes/My Shared Files/project' 2>/dev/null || cd "$HOME"; export PT_IN_VM_SESSION=1; exec "$SHELL" -l`. |
| 8 | Nothing bounds `supervisor.log` growth. | Truncate on supervisor start (one instance = one log lifetime); the file is per-project and short-lived. |
| 9 | Timeouts are scattered through the prose. | Centralize as exported constants in `internal/state` (or a tiny `internal/ptcfg`): boot 120 s, ssh-dial retry backoff 250 ms→2 s, session-poll 2 s, empty-debounce 3 s, heartbeat 5 s, graceful stop 30 s, creating-wait poll 250 ms. Every package references these; no literals. |
| 10 | Clock and process spawning are untestable if used directly. | Wave 0 defines `Clock` and `Runner` (exec) seams. Unit tests inject fakes; `time.Now`/`exec.CommandContext` appear in exactly one file per package. |
| 11 | **`tart stop --force` does not exist.** Spec §6.3 step 6 and §10 both call for it; tart 2.32.1 answers `Unknown option '--force'` with exit 64. Verified against the installed binary in wave 1. | `tart.Client.Stop(ctx, name, force)` keeps its signature — the wrapper translates. `force=true` emits `tart stop <name> --timeout 0`; `force=false` emits a bare `tart stop <name>`, which uses tart's own 30 s graceful default (equal to `ptcfg.GracefulStopTimeout` by coincidence, not by wiring). Nothing above the wrapper knows the difference. |
| 12 | `RunOpts.NoGraphics` is honored rather than forced, per its frozen doc comment. | **The supervisor must set `NoGraphics: true` explicitly.** Omitting it opens a UI window for every VM pt boots — a silent, highly visible regression that no unit test against `tart.Fake` will catch. |
| 20 | **Spec §5.1's "`pt _check-trust` should complete in <10 ms" is a budget almost entirely spent before `main` runs.** Measured over 100–200 invocations each: `pt _check-trust` on a trusted project 8.7–9.6 ms; `pt --version` (full cobra tree) 9.2 ms; a 3.4 MB Go binary whose `main` is only `os.Exit(0)` **9.98 ms**; the same binary with `huh` linked 9.01 ms; `/usr/bin/true` 3.66 ms. The spread between those is inside the noise. | The implementation does the minimum real work — no YAML parse, no validation, no locks — and `main` serves the subcommand before building the command tree. But the finding is that **no Go implementation can meet 10 ms with margin in this environment**, because Go runtime startup plus dynamic loading costs ~6 ms over a ~3.7 ms process floor, and package-level `init` runs for every linked package regardless of which path `main` takes. This sandbox's process floor is unusually high (`true` is typically ~1 ms on bare hardware), so real-world numbers are likely well under budget. **Wave 5 must re-measure outside the sandbox** before this is called met or missed. |
| 19 | **Two packages now know the lock-file layout.** `state.RLock` waits `ptcfg.LockTimeout` (10 s), which is the wrong budget for a status command — N wedged projects would cost N×10 s — so `ports.GlobalRows` takes the shared flock directly with a short deadline. `state` exposes no short-wait variant. | Fix at the wave-2 gate by ADDING `state.Store.TryRLock(projectID, wait)` — additive, so the frozen contract still holds — and switching `ports.GlobalRows` to call it. Layout must be encoded in exactly one package. |
| 17 | Item 3's "fall back once to `vmIp:<vmPort>`" cannot live in `sshx.Forward` — the frozen signature takes a single `remoteAddr` and knows nothing about the guest's address. | **Wave 2 agent G (supervisor) owns the fallback**: probe `127.0.0.1:<vmPort>` and `vmIp:<vmPort>` once at tunnel setup and pass the winner to `Forward`. Doing it per-connection inside the tunnel would double the latency of every dial to a service that simply is not listening. |
| 18 | `sshx.TestServer` is exported in the ordinary build, so a working SSH *server* links into the shipped `pt` binary. | Accepted for now — it is what makes tunnels testable without a VM. Flagged for the wave-4 review to decide whether it moves behind a build tag; doing so would change the frozen contract, so it is not a wave-1 or wave-2 change. |
| 15 | **GC can delete the VM of a shell that is still booting it.** Per item 1, `pt shell` writes `instance.json{state:creating}` *before* spawning the supervisor, so a healthy instance briefly has no `supervisorPid`. A naive "PID not alive → reclaim" reads that window as a crash. | A record with `SupervisorPID <= 0` is spared until `CreatedAt + ptcfg.BootTimeout`, then reclaimed. **Wave 2 agent H must still write `SupervisorPID`/`SupervisorStart` as soon as the spawn returns** — the grace period is a crash backstop, not a license to leave the field unset. |
| 16 | §10 scopes reclamation to "dead supervisor with `state != dead`", but §6.1 has `Dead → NoInstance` on next shell or GC. The two overlap inconsistently. | Unified: **the supervisor being dead is the entire predicate**, whatever the state field says. A live-but-wedged supervisor (stale heartbeat, live PID) is deliberately left alone — killing a running process's VM is worse than showing a stuck row in `pt list`. |
| 14 | Spec §3.1 requires `resources.cpu ≥ 1`, but the frozen `Resources` fields are `omitempty` with "zero means inherit" — so after decoding, `cpu: 0` and an absent `cpu` are indistinguishable. Rejecting `0` would make `resources: {memory: 8192}` fail. | `0` means inherit; only negative values are rejected (and `0 < memory < 512`). Literal `cpu: 0` therefore passes validation. Changing this requires presence tracking via `yaml.Node`, which is not worth it. |
| 13 | `tart list --format json` reports `"Source": "OCI"` (uppercase) while the frozen `SourceOCI` constant is `"oci"`. | `tart.List` lowercases `Source` and `State` after decoding. Consequence: never `json.Unmarshal` into `tart.VM` directly — go through `List`, or the comparison against the constants silently fails. |

---

## 2. Contracts (wave 0 output — the thing that makes parallelism safe)

A single agent writes **compiling, tested-to-`go vet` skeletons** for every package before
any implementation agent starts. Bodies are `panic("TODO(wave1)")`; signatures, structs,
and doc comments are final. This is the interface freeze — wave 1+ agents implement against
it and may not change exported signatures without escalating to the orchestrator.

Three packages exist that the spec's §11 layout does not name, all added in wave 0:

- `internal/ptcfg` — every timeout, poll interval and debounce as named constants.
  No other package may hard-code a duration.
- `internal/sys` — the `Clock` and `Runner` seams, plus `FakeClock`/`FakeRunner`/
  `FakeProcess`. **Implemented in wave 0, not stubbed**: every other package's tests
  depend on it, so leaving it a skeleton would have serialized wave 1 behind one agent.
- `internal/deps` — blank imports pinning the runtime dependencies. Without it
  `go mod tidy` drops libraries no implemented package imports yet, and parallel agents
  race each other on `go.mod`. Delete it once every library is genuinely imported.

```
internal/config    Config, Mount, Port structs; Load(dir) (*Config, RawBytes, error);
                   (*Config).Validate() error; Find(startDir) (projectDir string, error);
                   HashBytes(raw []byte) string  // "sha256:<hex>"
internal/trust     Store interface{ Check(path, hash) (bool, error); Allow(path, hash) error;
                   Get(path) (Record, bool, error) }; Open(stateDir) (Store, error)
internal/tart      Client interface{ Clone, Set, Run, Stop, Delete, IP, List };
                   VM struct; RunHandle interface{ Wait() error; Kill() error };
                   NewCLI(Runner) Client; NewFake() *Fake
internal/state     Dir layout helpers; ProjectID(canonicalPath) string;
                   Store interface{ WithLock(fn), WithRLock(fn), ReadInstance, WriteInstance,
                   AddSession, RemoveSession, ListSessions, GC(tart.Client) error };
                   Alive(pid int, startTicks uint64) bool
internal/sshx      Dial(addr, user, pass, Clock) (*Client, error) with retry;
                   (*Client).Interactive(cmd string, term *os.File) (exitCode int, err error);
                   (*Client).Tunnel(ctx, hostAddr, remoteAddr) (io.Closer, error)
internal/ports     Negotiate(cfg []config.Port, interactive bool, io) ([]Resolved, error);
                   Render(w, []Resolved, status) error
internal/supervisor Run(ctx, Params) error        // Params decoded from stdin JSON
internal/shell     Run(ctx, Opts) (exitCode int, error)
internal/zshplugin //go:embed pt.plugin.zsh; Script() string
cmd/pt             cobra root + init/allow/shell/ports/list/_supervise/_check-trust/zsh-hook
```

Also in wave 0: `Makefile` with `make check` = `gofmt -l . && go vet ./... && go build ./... &&
go test ./...`. Every subsequent agent must leave `make check` green — that is the gate.

---

## 3. Wave structure

```mermaid
graph LR
    W0[W0: contracts<br/>1 agent, serial] --> A[config]
    W0 --> B[trust]
    W0 --> C[tart]
    W0 --> D[sshx]
    W0 --> E[state]
    A & B & E --> F[ports]
    C & D & E --> G[supervisor]
    A & B & D & E & F --> H[shell]
    F & G & H --> I[W3: cmd wiring + init picker]
    W0 --> J[W3: zshplugin]
    I & J --> K[W4: review + docs]
    K --> L[W5: integration + smoke]
```

Waves are barriers: run `make check` and a quick human skim between them. Within a wave,
agents run concurrently and own disjoint directories — no agent edits a file outside its
package, and only the wave-0 and wave-3 agents touch `cmd/pt`.

### Wave 0 — contract freeze (1 agent, serial, ~30 min)

Deliverable: everything in §2, compiling, `go.mod` tidied with the spec's libraries
(`spf13/cobra`, `charmbracelet/huh`, `gofrs/flock`, `golang.org/x/crypto/ssh`,
`golang.org/x/term`, `gopkg.in/yaml.v3`). No behavior. Acceptance: `make check` green with
`panic` bodies untested, `go doc ./internal/...` reads coherently.

### Wave 1 — leaf packages (5 agents, parallel)

| Agent | Package | Deliverable | Acceptance |
|---|---|---|---|
| **A** | `internal/config` | Strict YAML decode (`KnownFields(true)`), all §3.1 validation rules, `~`/relative path expansion, upward project search bounded at `/`, byte-exact hashing. | Table-driven tests covering every validation rule *and its negative case*; `mounts[].name: project` with `host_path` set must error; duplicate `host_port` must error; unknown key must error. |
| **B** | `internal/trust` | `trust.json` load/store, atomic temp+rename under `flock`, path canonicalization at the boundary. | Tests: concurrent `Allow` from N goroutines leaves valid JSON; hash mismatch → not trusted; missing file → not trusted, not an error. |
| **C** | `internal/tart` | Exec wrapper over the real CLI (`--format json` where supported), plus `Fake` with scripted responses and call recording. | Tests drive `Fake`; CLI impl tested by asserting the exact argv produced for each method against a recording `Runner`. |
| **D** | `internal/sshx` | Dial-with-backoff, interactive PTY session (raw mode, `TERM` + window size, `SIGWINCH` forwarding, exit-code propagation), tunnel listener (loopback-only bind, per-conn goroutine, clean `Close`). | Tunnel tested against an in-process `ssh` server from `x/crypto/ssh` (no VM needed) — this is the one non-obvious test and is worth the effort. Interactive session tested for arg construction + raw-mode restore on panic. |
| **E** | `internal/state` | Dir layout, `flock` wrappers with short-hold discipline, instance/session record IO, PID+startTicks liveness (`KERN_PROC_PID` sysctl), full §10 GC including orphaned `pt-*` VM sweep. | Tests: GC removes dead-PID session files; GC force-deletes VMs whose supervisor is dead; GC **never** touches VMs not matching `^pt-[0-9a-f]{16}-[0-9a-f]{8}$`; N-goroutine race registering/removing sessions leaves a consistent dir. |

### Wave 2 — orchestration (3 agents, parallel)

| Agent | Package | Notes |
|---|---|---|
| **F** | `internal/ports` | Probe-bind each `host_port`; on `EADDRINUSE` prompt with default `port+10000` if free else `net.Listen(":0")`; re-prompt on collision; non-TTY → auto-select and print. Renders the `pt ports` table (both scopes). Tests inject an `io.Reader`/`io.Writer` pair and a pre-bound port. |
| **G** | `internal/supervisor` | Decode params from stdin; clone → `set` (only if overridden) → `run` with `--no-graphics --dir=...`; poll `tart ip` + TCP `:22` to 120 s; open tunnels; write `state:running`; then the three concurrent watchers (heartbeat 5 s, session-dir 2 s w/ 3 s empty-debounce, `tart run` child); teardown per §6.3 exactly. Tests run the whole lifecycle against `tart.Fake` + fake clock + a temp state dir, asserting the state sequence `creating→running→stopping→dead` and that teardown is idempotent under a mid-flight child exit. |
| **H** | `internal/shell` | Resolve project → verify trust → GC → lock → decide create/attach → (create path: validate mounts, negotiate ports, write record, spawn detached `Setsid` supervisor with stdio to `supervisor.log`) → (attach path: release lock, poll for `running` with spinner + 120 s timeout, print the config-drift note when `configHash` differs) → register session → SSH interactive → deregister under lock → mirror exit code. Tests cover both paths plus the "supervisor died during creating" and "state == stopping → wait for dead, then recreate" branches. |

### Wave 3 — surface (2 agents, parallel)

| Agent | Scope |
|---|---|
| **I** | `cmd/pt`: wire every subcommand, `--json` for `list`/`ports`, `-v/--verbose`, exit codes (incl. `_check-trust`'s 0/10/1); `pt init` interactive flow with the `huh` image picker fed by `tart list --format json` + free-text OCI entry, port prompts, commented-YAML emission, auto-allow; `pt list` column assembly (`ps` for CPU/RSS over the `tart run` tree, `du -sk`, uptime). |
| **J** | `internal/zshplugin`: `pt.plugin.zsh` (`chpwd` + `precmd`, bounded upward walk, no-op if `pt` absent, `PT_PROMPT` prefix, green/yellow), embedded and printed by `pt zsh-hook`. Must include a measured note that `pt _check-trust` stays under 10 ms — benchmark it. |

### Wave 4 — hardening (2 agents, parallel)

| Agent | Scope |
|---|---|
| **K** | Adversarial review of waves 1–3 against the spec, section by section: every MUST in §§3–10 mapped to the code that implements it and the test that covers it. Output a gap list, not edits. |
| **L** | `README.md`: install, `pt init` walkthrough, the Linux-guest mount caveat (§7), `PT_SSH_USER`/`PT_SSH_PASSWORD` escape hatch, security model (why `InsecureIgnoreHostKey` is acceptable here), troubleshooting via `supervisor.log`. |

### Wave 5 — reality check (serial, 1 agent + human)

`//go:build integration` test that clones a real image, boots, forwards a port, opens a
session, exits, and asserts the clone is gone. Then a manual smoke pass: two concurrent
`pt shell`s sharing one VM, `pt list` mid-session, `pt ports` with a deliberate host-port
collision, `kill -9` the supervisor and confirm the next `pt shell` recovers cleanly.

**This wave is where the design gets falsified.** Budget real time for it; the first four
waves are all testable against fakes and will feel deceptively finished.

---

## 4. Agent execution mechanics

- **Model/effort:** Opus 5 for every wave. High effort for waves 0, 2, 4 (contract design,
  concurrency, review); default effort is sufficient for waves 1, 3, 5.
- **Isolation:** shared working tree, *not* worktrees. Package ownership is already disjoint,
  and worktree-per-agent adds merge cost for no conflict avoided. The one rule: an agent that
  believes it needs to edit outside its package must stop and report instead.
- **Prompt template** (per agent): spec section references it must satisfy → the frozen
  interface it implements → its exclusive file list → "leave `make check` green" → "write
  tests in the same commit" → "report any spec ambiguity rather than resolving it silently."
- **Between waves:** orchestrator runs `make check`, reads the diff, and resolves anything
  escalated before releasing the next wave.
- **Do not** run waves 1 and 2 concurrently. The temptation is real — wave 2's agents will
  compile against wave 1's skeletons — but they cannot be meaningfully tested against
  `panic()` bodies, and debugging that is more expensive than the barrier.

---

## 5. Risks

| Risk | Mitigation |
|---|---|
| ~~Tart image pull gates all real verification.~~ | Resolved — `macos-tahoe-base` is pulled. |
| The supervisor is the only genuinely concurrent component; races there are the most likely source of production bugs. | Fake clock + deterministic lifecycle tests in wave 2; `go test -race` in `make check`. |
| `crypto/ssh` interactive PTY handling (raw mode, resize, exit codes) is fiddly and only fully exercised against a real VM. | Wave 1D builds against an in-process SSH server; wave 5 confirms against the real thing. |
| Spec §12 forbids revisiting six decisions; agents may "improve" them anyway. | State the frozen decisions verbatim in every agent prompt. |
| `pt shell`'s create path holds user attention through port negotiation *and* a 120 s boot. | Ensure the spinner and prompts are on the interactive process only; supervisor stdio goes to the log, never the terminal. |

---

## 7. Wave-4 audit findings

The spec-conformance audit produced 17 findings. Those fixed are listed with the
commit that closed them; those recorded are deliberate, and each names why.

### Fixed

| # | Finding | Resolution |
|---|---|---|
| F1 | `pt ports` never garbage-collected, though spec §10 names it as a GC site alongside `shell` and `list`. Worse, `GlobalRows` filters dead-supervisor projects *out of the display*, so orphans were neither reported nor reclaimed — disk filled silently. | GC added to `runPorts`, non-fatal, on stderr. |
| F2 | Every `pt list` column was untested, behind a test named `TestListTableLabelsDiskApproximate` that asserted only the **empty-store** path — where the header and footnote are never emitted. It could not fail on the thing it was named for. | Replaced with `list_test.go`: all eight columns, the `DISK*` footnote, the STATE-override, the `--json` key set, and a GC-warning-does-not-corrupt-JSON case. |
| F5 | Two simultaneous first-run shells both probed host ports; the second saw its own sibling's probe as `EADDRINUSE` and prompted the user about a conflict that did not exist, then discarded the answer. | `create` now claims the project **before** negotiating ports. Only the winner ever prompts. |
| — | **Found by F5's test, not by the audit:** `state.acquire` did `MkdirAll` then opened the lock file inside it. A supervisor's teardown removing the project directory between those two steps killed an *unrelated* pt invocation with a bare `no such file or directory`. | A vanished lock file (`ENOENT`, or `EINVAL` when it is unlinked and replaced under an open descriptor) is now treated as contention and retried within a short bounded window. |
| F7 | `TryRLock`'s doc claimed `pt shell`'s boot poller used it and therefore could not resurrect state. The poller uses `RLock`, which creates. | Doc corrected to state what is actually true, including the consequence. |
| F8 | `ptcfg.CheckTrustBudget` documented enforcement "by a benchmark"; no benchmark existed anywhere. | `BenchmarkCheckTrust` added: **0.125 ms/op**, 80× under the 10 ms budget, confirming item 20 that the budget is spent on process startup rather than on this code. |
| F9 | `statusLockWait` was declared twice, in `ports` and in `cmd/pt`, equal only by coincidence — against items 9 and 19. | One `ptcfg.StatusLockWait`. |
| F14 | `internal/deps` was dead; plan §2 said to delete it once every library was genuinely imported. | Deleted. |
| F16 | `TestRecoversFromKilledSupervisor` could skip itself, so a green run was not evidence the backstop worked. | Now snapshots the orphan immediately after the kill and fails if it is absent. Passes against a real VM. |

### Recorded, not fixed

| # | Finding | Why |
|---|---|---|
| F3 | Spec §7's "print a hint if the login banner suggests a Linux guest" is unimplemented. | The functional fallback (`cd "$HOME"` when the share is absent) works and is tested. The README documents the gap explicitly rather than a best-effort banner sniff being added late. |
| F6 | `chooseRemoteAddr` probes `vmIp:<port>` from the **host's** stack at tunnel-setup time, when nothing is listening on either candidate. The `127.0.0.1`-vs-`vmIp` fallback of items 3/17 is therefore dead code in practice. | Real, and the audit rates it top-3. The fix needs an additive `sshx` entry point (the frozen `Forward` takes one address) plus lazy per-tunnel latching. **The consequence today: a guest service bound only to the external interface, started after boot, is not reached.** Worth doing next. |
| F10 | `state.Heartbeat` is the one unlocked mutation of project state, against §3.3. | Safe today, and teardown already works around it by stopping the beat before removing state. Documenting the exemption is honest; adding a lock to a 5-second timer is not obviously an improvement. |
| F11 | `pt _supervise` boots from its stdin params without consulting `trust.json` or re-checking `ConfigHash`. | Not a `pt shell` bypass — anyone who can run `pt _supervise` can run `tart` directly, so it grants no new capability. But it means trust is a single check rather than a layered one. A cheap hardening exists (re-hash the config, refuse on mismatch) and should be a deliberate decision, not an accident. |
| F12 | `-v/--verbose` is global but read at exactly one site. | Either wire it or scope it to `shell`; both are real work and neither is a defect. |
| F13 | `pt ports` emits a `stale` status the spec does not define, and it lands in the documented `--json` contract. | Good addition, well tested. Recorded here so the spec and the JSON consumers agree it exists. |
| F15 | The exclusive project lock is held across `tart stop`/`tart delete` subprocesses with no deadline, the longest hold in the system. | A wedged `tart` therefore blocks every other invocation for that project until `LockTimeout`. Bounding the context is the fix. |
| F17 | `trust.json` uses a sidecar lock rather than spec §5's "flock on the file". | The implementation is *better* than the spec text — an flock on an inode that `rename` replaces serializes nothing — but it is a deviation from a normative sentence in the security section, so it is recorded here. |
| F18 | `sshx.TestServer` links a working SSH server into the shipped binary. | The audit **disproved** this by inspecting the built binary: all 9,928 symbols, zero server-side matches. Go's linker eliminates it because nothing reachable from `main` refers to it. Moving it to `internal/sshxtest` is still tidier — "the linker saves us" is not a property anyone re-verifies — but it is not a live defect. |

## 6. Definition of done

`make check` green; every §§3–10 MUST traced to an implementation and a test (wave 4K's
matrix); the wave 5 manual pass completes without leftover `pt-*` VMs in `tart list` or
stale directories under `~/.local/state/plasticturtle/instances/`.
