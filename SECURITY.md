# Security policy

## Reporting a vulnerability

Report it privately through GitHub: on the repository's **Security** tab, choose **Report a
vulnerability** (GitHub's private vulnerability reporting). Please do not open a public issue for
it. Include the coreslot version (`coreslot version`), the operating system and architecture, and
the smallest sequence of commands that shows the problem.

You will get an answer in the advisory thread. A confirmed issue is fixed in a release, and the
advisory is published with it, crediting you unless you ask otherwise.

Only the latest release is supported while coreslot is at `v0`
([compatibility policy](docs/compatibility.md)).

## Threat model

coreslot is **cooperative admission control for one user on one host**. It is not a security
boundary and does not enforce anything.

- **Cooperative.** A process receives a grant because it asked. Nothing stops a process that never
  asks, or one that uses more cores than it was granted: coreslot sets no cgroup quota, `nice`
  value or CPU affinity.
- **Same user, trusted.** The state directory belongs to one user, and every process of that user
  can read and write it. Any same-user process can hoard tokens (lock them and never release),
  stop inside the admission section and block every admission (`coreslot status` names it), or
  corrupt the state. coreslot detects many forms of corruption and fails open or refuses, but it
  does not defend against a hostile process of the same user. Such a process already has the
  user's whole account.
- **One host, one user.** Never share the state directory across users or hosts. Locks on network
  filesystems are not host-local, and a second user's processes are not part of the budget.
  coreslot refuses a state directory on a network, FUSE or VM-shared filesystem (NFS, SMB, AFP,
  WebDAV, macFUSE, 9p, vboxsf, Ceph, AFS). Multi-user hosts are not supported in v1.

### State directory policy

The full rules are in
[`PROTOCOL.md` section 4](PROTOCOL.md#4-state-directory-and-filesystem-policy); in short:

- The directory is `~/Library/Application Support/coreslot/` on macOS and `~/.local/state/coreslot/`
  elsewhere, where `~` comes from the password database (`getpwuid(getuid())`), never from `$HOME`
  or `XDG_*`. A cgo-free build (the release archives) whose uid has no entry in `/etc/passwd` asks
  NSS by running `getent` from a fixed system path, never one found through `$PATH`.
  `CORESLOT_STATE_DIR` (an absolute path) overrides it for tests and moves the configuration with
  it. Never point `CORESLOT_STATE_DIR` below a directory another user can write to:
  coreslot follows the ancestors of the state directory without checking them, so that user could
  replace one of them.
- Every directory coreslot creates is mode `0700`, every file `0600`, created with `O_NOFOLLOW`.
- Every existing path of the layout must be of the expected type, owned by the real user id, not a
  symlink, and not group- or world-writable; anything else is a state error. coreslot then fails
  open (runs the command unbudgeted and says so) or, with `--strict`, refuses. An entry inside
  `queue/`, `holds/` or `cancel/` that fails these checks is ignored and counted, never trusted or
  removed.
- A process whose effective user id differs from its real one (setuid) is unsupported.
- Descriptors on state files are close-on-exec, except the hold and token descriptors deliberately
  passed to the command, which keep the grant alive for as long as any process holds them.
- The run log (`log/runs.jsonl`) never records the command line or the environment. It does record
  labels, so a label must not carry a secret.
- Deleting or resetting the directory while anything is live creates a second budget;
  `coreslot doctor --reset` refuses unless nothing is live.

### Out of scope

Denial of service by a same-user process (hoarding, stopping inside the section), anything that
requires write access to the user's state directory or configuration, and a command that ignores
its grant. Reports about them are welcome as ordinary issues when coreslot could detect or report
the situation better.

coreslot sends no telemetry and opens no network connection.
