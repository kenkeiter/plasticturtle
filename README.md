# Plastic Turtle 🐢

Plastic Turtle is an easy-to-use sandboxing tool that allows you to work with your projects in dedicated, disposable [Tart](https://tart.run) VM instances. This is useful when you want to `--dangerously-skip-permissions`.

Using Plastic Turtle (`pt`) is simple:

1. **Add a `.plasticturtle` config to your project** – `cd` to your project directory and run `pt init`; you will be prompted to choose a base image and other parameters, and a `.plasticturtle` file will be created in your project directory.
2. **Play in the sandbox using `pt shell`** – Any time you are within your project directory, running `pt shell` will open up a new (SSH) connection to the VM (cloning and starting it, if it isn't already!) and give you a shell. VM instances are ephemeral, and are deleted after you close the last shell for your project; cloned VMs are copy-on-write, so you won't use more storage than you need to.

Couple of #protips: 

- Run `pt list` to see everything that's running.
- If you make changes to your project's `.plasticturtle` configuration, you must explicitly allow them by running `pt allow` within your project directory.

## What the isolation is and is not

What you get:

- A separate guest OS with its own kernel, filesystem, users and network stack,
  running under the Apple Virtualization framework.
- Exactly the host directories named in `.plasticturtle`, and no others. By
  default that is one directory: the project.
- Host ports opened only by explicit configuration, and only on `127.0.0.1`.
- No persistence. Every session group starts from a fresh clone of the image;
  writes inside the guest that are not under a shared directory are discarded
  at teardown.

What you do not get:

- **The project directory is read-write from the guest by default.** Anything
  running in the VM can rewrite your repo, including `.plasticturtle` itself and
  any git hooks, `Makefile`, or CI config in it. Set `mode: ro` on the reserved
  `project` mount if you do not want that. _Note_: If `.plasticturtle` _is_ modified, you must approve changes using `pt allow` before Plastic Turtle will allow futher interaction.
- **The guest has unrestricted outbound network access _by default_.** An agent
  in the VM can reach the internet, your LAN, and any service listening on your
  host's LAN address. A project can opt into a domain allowlist with a
  `network:` policy — see [Network firewall](#network-firewall) — which changes
  this to default-deny.
- No defense against a VM escape, and no hardening of the guest image beyond
  what the image ships with.
- Guest SSH host keys are not verified (see [Security notes](#security-notes)).

## Requirements

| | |
|---|---|
| Host | macOS on Apple Silicon (Tart is macOS/arm64 only) |
| `tart` | on `PATH`; developed against 2.32.1 |
| Guest image | `ghcr.io/cirruslabs/macos-tahoe-base:latest` is the tested image |
| Shell plugin | zsh (optional; `pt` itself works from any shell) |
| Build | Go 1.26+ |

v1 targets **macOS guests**. Linux guests boot and forward ports, but the
directory share is not auto-mounted; see [Linux guests](#linux-guests).

## Install

```sh
git clone https://github.com/kenkeiter/plasticturtle
cd plasticturtle
make build            # -> ./bin/pt and ./bin/pt-softnet-shim
cp bin/pt bin/pt-softnet-shim /usr/local/bin/
```

`pt-softnet-shim` is only needed for the [network firewall](#network-firewall);
keep it next to `pt` (`pt setup-firewall` looks for it there). If you do not use
`restricted` network policies you can skip it.

Pull the tested image once (first `pt shell` would otherwise pull it inline,
inside the 120 s boot timeout):

```sh
tart pull ghcr.io/cirruslabs/macos-tahoe-base:latest
```

Then add the zsh integration to `~/.zshrc`:

```sh
source <(pt zsh-hook)
```

The plugin is small and deliberately unobtrusive. It installs a `chpwd` hook and
a `precmd` hook that:

- walk upward from `$PWD` for a `.plasticturtle`, stopping at `$HOME` or `/`;
- run `pt _check-trust <dir>` when one is found (exit `0` trusted, `10`
  new/changed, `1` error);
- print a one-line yellow warning on `10`, once per directory change, not once
  per prompt;
- prefix `PROMPT` with a turtle — green when trusted, yellow when not — by
  exporting `PT_PROMPT` and prepending it, stripping its own previous prefix
  first so prompt frameworks that rewrite `PROMPT` are not fought;
- preserve `$?` across both hooks, tolerate `errexit`, and `return 0` silently
  if `pt` is not on `PATH` or the plugin is sourced twice.

## Quick start

```sh
cd ~/code/myproject
pt init      # pick an image, name any ports; writes .plasticturtle and allows it
pt shell     # clone, boot, and log in
```

`pt init` is interactive and refuses to run without a TTY or when a
`.plasticturtle` already exists. It offers the images `tart list` already has
locally (skipping running VMs and `pt-*` clones of its own) plus a free-text OCI
reference entry, then asks for port forwards as one comma-separated line
(`3000, 5432:15432`). It writes a commented `.plasticturtle` and records trust in
it without a second prompt — you just authored the file.

What the first `pt shell` does, in order: resolve the project, check trust,
garbage-collect stale state, bind the configured host ports (prompting if one is
taken), write the instance record, spawn a detached supervisor, wait at a spinner
for the VM, then hand the terminal to the guest's login shell.

Expect the first boot to take **about 30 seconds** with the tested image. The
clone itself is near-instant — it is APFS copy-on-write, not a copy.

You land here:

```
admin@guest ~ % pwd
/Volumes/My Shared Files/project
admin@guest ~ % echo $PT_IN_VM_SESSION
1
```

That directory *is* your project on the host, read-write in both directions by
default. A second `pt shell` in another terminal attaches to the same VM rather
than booting a new one. When the last one exits, the VM stops and the clone is
deleted; the only thing that outlives it is `trust.json`.

## `.plasticturtle`

Checked into the repo, human-edited, and inert until `pt allow` approves its
exact bytes.

```yaml
version: 1

image: ghcr.io/cirruslabs/macos-tahoe-base:latest

resources:
  cpu: 8            # vCPUs
  memory: 8192      # MiB

ports:
  - vm_port: 3000
    host_port: 3000
  - vm_port: 5432   # host_port defaults to 5432

mounts:
  - name: project   # reserved: changes the mode of the always-present project share
    mode: ro
  - name: datasets
    host_path: ~/datasets
    mode: ro
  - name: scratch
    host_path: ./scratch
    mode: rw
```

### Fields

| Field | Type | Required | Default | Rules |
|---|---|---|---|---|
| `version` | int | yes | — | must be exactly `1` |
| `image` | string | yes | — | non-empty after trimming; local VM name or OCI reference |
| `resources` | map | no | inherit from image | omit entirely to inherit both |
| `resources.cpu` | int | no | `0` = inherit | `0` or `>= 1`; negatives rejected |
| `resources.memory` | int (MiB) | no | `0` = inherit | `0` or `>= 512`; anything in `1..511` and negatives rejected |
| `ports[].vm_port` | int | yes | — | `1..65535` |
| `ports[].host_port` | int | no | same as `vm_port` | `1..65535`; duplicate *effective* host ports across entries are an error |
| `mounts[].name` | string | yes | — | `[a-zA-Z0-9_-]+`, unique within the file |
| `mounts[].host_path` | string | see rules | — | required for every mount **except** `project`, where it is **forbidden** |
| `mounts[].mode` | `rw` \| `ro` | no | `rw` | any other value is an error |
| `network.policy` | `open` \| `restricted` | required if `network` present | `open` (when block absent) | any other value is an error |
| `network.allow[]` | string | no | — | bare domain or `*.domain`; no scheme, port, path, or IP; forbidden under `open` |

Additional rules:

- **Strict decoding.** An unknown key at any level is an error, not a warning. A
  typo in a security-relevant file should cost you one run, not one incident.
- **One document.** A second `---` YAML document in the file is an error: it
  would be invisible to somebody reading the first.
- **`host_path` expansion.** `~` is the invoking user's home. `~otheruser` is
  rejected rather than silently treated as a relative directory. Relative paths
  resolve against the project directory, never the working directory. Every
  resolved path must exist and be a directory before the VM is cloned; a missing
  one is a hard error from `pt shell` (and a non-fatal warning from `pt allow`).
- **The `project` mount is implicit.** The project directory is always shared,
  first in the list, at `/Volumes/My Shared Files/project` in a macOS guest.
  Listing `name: project` in `mounts[]` only changes its `mode`; it may not
  redirect it elsewhere.

### `resources.cpu: 0` means inherit

The design document says `resources.cpu` must be `>= 1`. The implementation
does not, and deliberately: after YAML decoding, an absent `cpu` and a literal
`cpu: 0` are indistinguishable, so rejecting `0` would make the perfectly
reasonable `resources: {memory: 8192}` fail validation. `0` therefore means
"inherit from the image" and passes validation; only negative values are
rejected. The same holds for `resources.memory`. `tart set` is invoked only for
the fields that are non-zero, and not at all when both are.

## Trust

A `.plasticturtle` names the image a project boots, the host directories it
exposes, and the host ports it opens. That is a lot of authority for a file that
arrives with a `git clone` — or that an agent with write access to the repo can
edit between one `pt shell` and the next.

So the file does nothing until you approve it:

```
$ pt allow
.plasticturtle in /Users/alice/code/myproject

  image      ghcr.io/cirruslabs/macos-tahoe-base:latest
  resources  inherited from the image

  mounts
    project   /Users/alice/code/myproject  READ-WRITE
    datasets  /Users/alice/datasets        read-only

  ports
    VM 3000  -> host 3000  (reachable on 127.0.0.1 only)

Allow it? [y/N]:
```

Mechanics:

- Trust is `sha256` over the **exact bytes** of the file, stored in
  `trust.json` against the **canonical absolute path** of the project directory.
- Any change to the bytes — a comment, whitespace, a new mount — invalidates it.
  `pt shell` then fails with
  `.plasticturtle has changed (or was never allowed). Review it, then run: pt allow`
  and never with a prompt. A prompt at that moment would teach you to approve a
  file you have not read, which is the exact failure `pt allow` exists to
  prevent. The message does not distinguish "changed" from "never allowed"; the
  remedy is identical either way.
- Because the key is the canonical path, moving or renaming the project requires
  re-allowing, and a project reached through a symlink is the same project as one
  reached directly.
- `pt allow` validates first: an invalid config cannot be trusted, and you are
  not asked about something that could never work.
- Answering anything but `y`/`yes` — including EOF, i.e. a non-interactive run —
  declines and exits `1` with `Not allowed.` It does not print an error; you made
  a choice.
- `pt init` records trust without a confirmation prompt, because you just
  answered every question the summary would have shown you.

This is why the zsh plugin exists: it tells you the moment you `cd` into a
project whose config has changed, rather than at `pt shell` time when you are
already trying to get work done.

## Commands

```
pt init  [path]              Interactive setup; writes .plasticturtle and allows it
pt allow [path]              Show what the config grants, then trust its exact bytes
pt shell [path]              Enter the project's VM, creating it if needed
pt ports [path] [--global]   Configured forwards and their live status
pt list                      Active instances with resource usage
```

`path` defaults to the working directory. Except for `init`, `pt` walks upward
from it to the nearest `.plasticturtle`, so every command works from a
subdirectory.

Global flags: `--json` (honored by `list` and `ports`), `-v`/`--verbose`,
`--version`.

Hidden subcommands, listed for completeness — do not invoke them yourself:
`pt _supervise` (the detached per-instance supervisor, fed its parameters as
JSON on stdin), `pt _check-trust <dir>` (the plugin's fast trust probe),
`pt zsh-hook` (prints the plugin).

### `pt shell`

Creates the instance if there is none, otherwise attaches to the existing one.
Prompts about host-port collisions before the VM exists; shows a spinner while
one boots (and prints the label once, without animation, when output is not a
terminal).

If the running VM's snapshotted config differs from the currently allowed one,
it attaches anyway and prints
`note: config changed since this VM started; changes apply after all shells exit.`

`-v` additionally prints the instance name and the path of the supervisor log
on the create path — the easiest way to find that log.

Exit status:

| Status | Meaning |
|---|---|
| *n* | the remote shell exited with *n* |
| `1` | `pt` itself failed (no config, untrusted, missing mount, boot timeout) |
| `255` | the SSH session never happened or was lost mid-flight (ssh(1)'s convention) |

Diagnostics go to stderr, so the guest shell's stdout stays clean for redirection.

#### The terminal in the guest

When stdin is a terminal, `pt shell` gives the guest a PTY that mirrors the
host's: the same size, the same control characters (erase, interrupt, suspend,
flow control), and — where it can — the same `TERM`.

`TERM` needs a negotiation because terminals like Ghostty, kitty and WezTerm
ship a terminfo entry of their own and install it only on the host. A guest
handed a name it cannot resolve gets no cursor-movement capabilities, and the
first thing you notice is that **backspace stops erasing**. So `pt` asks the
guest whether it knows the name, and if it does not, compiles the host's own
description into `$HOME/.terminfo` in the guest with `tic`. If that fails — no
`tic`, no writable home, an entry the host cannot describe — the session falls
back to `xterm-256color`, which is plainer but always works.

The whole exchange is bounded by a short timeout and never blocks the prompt: a
slow guest costs you a plainer `TERM`, not a slower shell. Nothing is written
outside the clone, which is discarded at teardown.

### `pt list`

Runs a global garbage-collection pass, then reports every project with an
instance record.

```
$ pt list
PROJECT                       VM                            STATE    SESSIONS  CPU %  MEM     DISK*  UPTIME
/Users/alice/code/myproject   pt-e3b5380ebc1df727-e0342b0a  running  2         38.4   2.1G    29.8G  4m12s

* approximate: CoW clones share blocks with the source image.
```

| Column | Source |
|---|---|
| `PROJECT` | project path from `instance.json` |
| `VM` | clone name, `pt-<project-id>-<8 hex>` |
| `STATE` | `creating`, `running`, `stopping`, `dead` — forced to `dead` if the supervisor PID is not alive, whatever the record claims |
| `SESSIONS` | session records whose process is still alive |
| `CPU %` / `MEM` | summed from `ps` over the supervisor's process tree |
| `DISK*` | `du -sk` of `~/.tart/vms/<vm>` (honors `TART_HOME`) |
| `UPTIME` | now − `createdAt` |

**`DISK*` is approximate and usually wrong in an interesting way.** A fresh clone
occupies almost nothing, but the column will read something like `29.8G`, because
`du` charges copy-on-write shared blocks to whichever path it walks first — often
the clone rather than the base image. The asterisk and the footnote are there for
exactly this reason. No smarter accounting is attempted.

`--json` emits an array (never `null`) of objects with keys `project`, `vm`,
`state`, `sessions`, `cpuPercent`, `memBytes`, `diskBytes`, `uptimeSeconds`.

### `pt ports`

```
$ pt ports
VM PORT  HOST PORT  STATUS
3000     3000       forwarding
5432     15432      forwarding (remapped from 5432)
```

Status values:

| Status | Meaning |
|---|---|
| `forwarding` | instance is `running` and its supervisor's heartbeat is under 15 s old |
| `stale` | an instance record exists but the supervisor has stopped beating, or the instance is still `creating` / already `stopping` — the listener is not to be trusted |
| `inactive` | the project has no instance record; rows come from the config alone |

`(remapped from <n>)` is appended when the forward is not on the port the config
asked for.

`--global` needs no project: it sweeps every project with a live supervisor,
prefixes a `PROJECT` column, and appends `conflict: also claimed by <path>` when
two projects' records claim the same host port. Projects whose lock is busy are
skipped rather than waited on, so `--global` reports what it could see promptly
rather than everything eventually.

`--json` emits an array of objects with keys `projectPath`, `vmPort`, `hostPort`,
`originalHostPort`, `status`, `conflict`. Every key is always present.

## Ports

Forwards are SSH local forwards implemented inside the supervisor with
`golang.org/x/crypto/ssh` — no `ssh` binary, no extra processes. They die with
the supervisor by construction, so there is no orphaned listener to clean up.

- Listeners bind **`127.0.0.1` only**, never `0.0.0.0`. This is a sandboxing
  tool; a forward reachable from the LAN would defeat it. The bind address is a
  constant, not a setting.
- Inside the guest, each tunnel dials `127.0.0.1:<vm_port>` — a dev server bound
  to guest loopback is invisible at any other address. The one exception is
  probed once at setup: if `<vmIP>:<vm_port>` answers from the host, that address
  is used instead, covering services bound only to the guest's external
  interface. The choice is made once per forward, not per connection.
- A service listening in the guest is reachable on the host at
  `127.0.0.1:<host_port>`.

### Collisions

If a configured host port is already bound (or is a privileged port this user
cannot bind), `pt shell` — which still owns a terminal at that point — offers an
alternative before the VM is created:

```
Port 5432 is in use on the host.
Forward VM port 5432 to host port [15432]:
```

The default in brackets is `host_port + 10000` if that is free, otherwise a
kernel-assigned ephemeral port, and it is **already bound** while you read the
question, so it cannot be stolen out from under the prompt. Press Enter to accept
it or type any other port; a port that is also taken re-prompts. Without a TTY
there is no prompt: the automatic port is taken and reported.

`pt ports` then shows `forwarding (remapped from 5432)`.

**Remaps are never written back to `.plasticturtle`.** Trust is a hash of the
file's exact bytes, so writing to it would invalidate the hash and turn a routine
port collision into a security prompt. The remap lives in `instance.json` for the
lifetime of that instance and disappears with it.

The negotiation is done by `pt shell`, not the supervisor, because the supervisor
is detached and has nothing to prompt on. The shell holds each probe listener
until the instant before it spawns the supervisor, which then re-binds; that gap
is a race, and the supervisor retries once if it loses.

## Network firewall

By default the guest reaches anything. A project can restrict outbound traffic to
a named set of domains by adding a `network:` block to `.plasticturtle`:

```yaml
network:
  policy: restricted        # open (default) | restricted
  allow:
    - github.com            # exact host
    - "*.githubusercontent.com"   # any subdomain (not the bare parent)
    - registry.npmjs.org
```

Under `restricted`, egress is **default-deny**: a connection is allowed only to
an address the guest learned by resolving a name on the allowlist. Everything
else — a hardcoded IP, an out-of-band resolver, IPv6, a domain not listed — is
dropped. Disallowed names resolve to `NXDOMAIN`, so tools fail fast and legibly
instead of hanging. Nothing in the guest needs configuring: `curl`, `git`,
package managers, and any other TCP client work transparently for allowed
domains and simply cannot connect for the rest.

### One-time setup

Enforcement runs on the host, in a small shim that wraps
[Softnet](https://github.com/cirruslabs/softnet) (Tart's software-networking
layer). Install it once:

```sh
pt setup-firewall     # copies the shim into place; sudo makes it setuid-root
```

This is required only for `restricted` projects; `open` projects are untouched
and need no setup. Re-run it after upgrading pt. A project whose policy is
`restricted` **refuses to boot** if the shim is missing or misconfigured — the
firewall fails closed rather than silently letting traffic through.

### How it works

Tart resolves the `softnet` binary from its `PATH`; for a restricted project, pt
puts its shim there first. The shim spawns the real Softnet as a child and relays
the guest's ethernet frames between them, enforcing the policy by **DNS-pinning**:
it watches DNS answers for allowed names, records the returned IPs in a
short-lived (TTL-bounded) allow-set, and drops any guest frame whose destination
is not pinned. Root is used only to launch the child; the long-lived relay then
drops back to your user.

### What it does and does not stop

- **Covers all TCP (and UDP) to allowed domains**, not just HTTP — because it
  gates on the destination IP, not on parsing application traffic. There is no
  proxy to configure and no TLS interception.
- **IPv6 and QUIC to arbitrary hosts are blocked**; clients fall back to IPv4/TCP
  for allowed domains.
- **Shared CDN IPs are coarse.** An allowed and a disallowed domain served from
  the same IP are indistinguishable once pinned; the allow-set is by address.
- **DNS itself is a residual channel.** The guest can still send queries to the
  gateway resolver (that is how resolution works at all), so a determined agent
  can exfiltrate small amounts of data by encoding it in DNS lookups. The
  firewall restricts where the guest can *connect*, not what it can *ask to
  resolve*.
- **Subnet collisions.** Software networking assigns the sandbox a private
  subnet; if it lands on the same range as your LAN, host connectivity breaks.
  pt detects this after boot and refuses with an explanation rather than leaving
  you with mysteriously broken networking.

## The guest filesystem

Shares are Tart virtiofs directory shares (`tart run --dir=<name>:<hostPath>[:ro]`).

On a macOS guest they appear under `/Volumes/My Shared Files/<name>`, so the
project is at `/Volumes/My Shared Files/project`.

`pt shell` does not use SSH environment forwarding (sshd rejects unlisted
`AcceptEnv` variables, silently). Instead it runs, in place of a bare login:

```sh
cd '/Volumes/My Shared Files/project' 2>/dev/null || cd "$HOME"; export PT_IN_VM_SESSION=1; exec "${SHELL:-/bin/sh}" -l
```

`exec`, so your login shell owns the PTY directly: job control, `SIGWINCH` and
the exit status all belong to it. `PT_IN_VM_SESSION=1` is there for nested
tooling that wants to know it is inside a sandbox.

### Linux guests

v1 targets macOS guests. A Linux guest boots and forwards ports fine, but its
kernel does not auto-mount virtiofs shares, so the `cd` above falls through to
`$HOME` and you must mount by hand:

```sh
sudo mkdir -p /mnt/shared
sudo mount -t virtiofs com.apple.virtio-fs.automount /mnt/shared
cd /mnt/shared/project
```

All shares appear as subdirectories of that one mount point, named by their
`mounts[].name`. To make it permanent inside an image you control, add to
`/etc/fstab`:

```
com.apple.virtio-fs.automount /mnt/shared virtiofs defaults 0 0
```

`pt` does not detect Linux guests and will not warn you; the design document's
"print a hint if the login banner suggests Linux" is not implemented.

## SSH credentials

`pt` logs into the guest as `admin` / `admin`, the Tart image convention. Both
`password` and `keyboard-interactive` are offered, because sshd images differ in
which one they advertise and a client offering only the wrong one fails with an
opaque "no supported methods".

Override with environment variables:

```sh
PT_SSH_USER=builder PT_SSH_PASSWORD=hunter2 pt shell
```

An empty value is treated as unset — `PT_SSH_USER= pt shell` is far more likely
to be an accident than a request to log in as nobody.

These are **deliberately not `.plasticturtle` fields.** That file is checked into
the repo and shared with everyone who clones it; credentials do not belong there.
Making them configurable per project would also make a hostile config able to
name the account it wants.

## Security notes

**Host keys are not verified.** `pt` uses `ssh.InsecureIgnoreHostKey()`. The
reasoning, from `internal/sshx/sshx.go`: these are ephemeral VMs that `pt` itself
just cloned, reachable only over the local virtio network, with no stable
identity to pin — a `known_hosts` entry would be regenerated on every boot and
would train the user to click through mismatches. The threat this drops is an
attacker already on the host's virtio network impersonating a VM that exists for
minutes, and that is not a threat this tool defends against.

**The trust boundary is `pt allow`.** Everything downstream of it — which image,
which host directories in which mode, which host ports — is whatever the file
said at the moment you approved it. Guest-side compromise is *outside* the
boundary and expected; a config change is *inside* it and must be re-approved.
Read the summary. It is not a formality.

**Config is snapshotted at boot.** The supervisor receives the fully resolved
config on stdin when it is spawned, and keeps it for the instance's lifetime.
Editing and re-allowing `.plasticturtle` while a VM is running changes nothing
about that VM: its image, mounts, modes and forwards are fixed. Changes take
effect the next time an instance is created, i.e. after every shell has exited.
This is also why a running VM cannot be made to mount a new host directory by an
agent editing the config from inside it.

**State files are private.** The state tree is `0700`/`0600`: it names project
paths and forwarded ports.

**Garbage collection only ever deletes VMs whose names match**
`^pt-[0-9a-f]{16}-[0-9a-f]{8}$` and that no instance record claims. Any
uncertainty — a busy project lock, an unreadable record — is resolved as "leave
it alone". Your other Tart VMs are never touched.

## State on disk

```
~/.local/state/plasticturtle/          # or $XDG_STATE_HOME/plasticturtle
├── trust.json                         # canonical project path -> {hash, allowedAt}
├── trust.json.lock
└── instances/
    └── <project-id>/
        ├── lock                       # flock guarding this project's state
        ├── instance.json              # the current instance record
        ├── heartbeat                  # mtime touched every 5 s by the supervisor
        ├── supervisor.log             # truncated at each boot
        └── sessions/<session-id>.json # one per live pt shell
```

`<project-id>` is the first 16 hex characters of `sha256(canonical project
path)`, and it is embedded in the VM name, so the easiest way to find a
project's directory is to take the middle field of its `VM` column in `pt list`.

There is no daemon. Every `pt` invocation reconstructs the world from these files
plus PID liveness checks; every record that names a PID also records that
process's start time, so a reused PID after a reboot is not mistaken for a live
supervisor. Mutations happen under the project's exclusive `flock`, never held
across a prompt or a boot.

## Troubleshooting

### Read the supervisor log

`~/.local/state/plasticturtle/instances/<project-id>/supervisor.log` is the only
record of a failed boot — the supervisor is detached and has no terminal. Every
error `pt shell` prints for a boot failure names the path. `pt shell -v` prints
it too, on the create path. It is truncated at the start of each boot, so it
always describes the most recent attempt.

It logs the clone, the resource override, the `tart run` child's pid, the DHCP
address, when sshd started accepting, each forward and which guest address it
chose, and the teardown reason.

### What `pt list`'s STATE means

| State | Meaning | What to do |
|---|---|---|
| `creating` | the supervisor is cloning/booting | wait; the boot timeout is 120 s |
| `running` | booted, tunnels up, SSH available | — |
| `stopping` | the last session exited; teardown in progress | wait; the next `pt shell` waits for `dead` on its own (up to 45 s) then makes a fresh VM |
| `dead` | the record's supervisor process is gone | the next `pt shell`, `pt list`, or GC pass reclaims it |

A row that says `running` with a `stale` status in `pt ports` is a supervisor
whose heartbeat has aged past 15 s. `pt` deliberately does not kill it: a
live-but-wedged process's VM is not something a status command should destroy.

### Recovering a wedged instance

Almost never necessary — `pt shell` and `pt list` both garbage-collect first —
but in order of escalation:

```sh
pt list                     # runs a global GC pass, then reports
```

If the supervisor was killed (`kill -9`), the VM is orphaned but harmless: the
next `pt shell` for that project notices the dead PID under the lock, force-stops
and deletes the leftover clone, clears the state, and boots fresh. `pt list` and
its GC pass do the same without booting anything.

If a supervisor is alive but wedged, GC will not touch it by design. Stop it
yourself, then let GC clean up:

```sh
kill <supervisorPid>        # from instance.json; SIGTERM means "tear down now"
pt list                     # reclaims the record and the clone
```

Last resort, if a clone survives everything above:

```sh
tart list                   # pt clones are named pt-<16 hex>-<8 hex>
tart stop --timeout 0 pt-<...>
tart delete pt-<...>
rm -rf ~/.local/state/plasticturtle/instances/<project-id>
```

### Other symptoms

| Symptom | Cause |
|---|---|
| `.plasticturtle has changed (or was never allowed)` | the bytes differ from what was allowed, or the project moved. Read the diff, then `pt allow` |
| `no .plasticturtle found at or above <dir>` | not in a project; `pt init` |
| `mount "x": host path ... does not exist` | a `host_path` is missing. `pt allow` warns about this; `pt shell` refuses |
| `the VM did not become ready within 2m0s` | image still pulling, or the guest never got a DHCP lease. Check the log; pre-`tart pull` the image |
| `VM terminated unexpectedly; see .../supervisor.log` | the `tart run` child exited under a live session |
| a stray `pt-*` VM in `tart list` | run `pt list`; the orphan sweep deletes `pt-*` VMs no record claims |
| backspace does not erase, arrow keys print escapes | the guest could not be taught your `TERM` and fell back. Check `infocmp "$TERM"` inside the guest; a guest image without `tic` cannot be taught one. `TERM=xterm-256color pt shell` sidesteps it |
| `DISK*` shows ~30G for a VM that just booted | expected. See [`pt list`](#pt-list) |
