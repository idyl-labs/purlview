# The local daemon: lifecycle, private control protocol and update checks

Purlview is one executable. Its `share` command starts a per-user daemon that
owns shares and their outbound QUIC/TCP connections to Purlview's edge; the CLI
controls it through private IPC.

The daemon creates shares through the account API and serves them through the
edge. A share is printed as ready only once the edge has admitted it. A lost
connection is re-established with the same URL and the original expiry. A
build with no platform configured says so instead of pretending to share.

## Process model

```text
purlview share ...  ──private socket/pipe──▶  purlview daemon run   (one per OS user)
purlview daemon *   ──private socket/pipe──▶        │
                                                     └── outbound QUIC/TCP platform connection
```

- One daemon per OS user. It is an ordinary user process: no root, no
  administrator elevation, no system service, no login item, no boot
  registration. A reboot or OS session termination that kills the daemon ends
  its shares; the next share starts a new daemon. (`purlview logout` is
  separate: it signs this device out locally.)
- The first `share` starts it and waits for a readiness handshake. Later
  commands reuse it. A daemon that is gone (crash, idle exit, OS session termination) is
  started again on the next share; nothing it owned comes back.
- The daemon runs the installed executable itself: `purlview daemon run
  --instance <id> --idle-exit <duration>`. Its working directory is the
  runtime directory, standard input is closed, standard output and error
  go to the log file. On macOS and Linux it is a new session leader with no
  controlling terminal; on Windows it is created with `DETACHED_PROCESS`
  and its own process group (breaking away from the caller's job object when
  the job permits it). Closing the terminal that ran the first share does
  not end the daemon or its detached work.
- **Idle exit:** with no connected client and no owned work the daemon exits
  after **60 seconds**. It never exits while it owns work.
- Routine startup prints nothing. Failures print one error naming the log.

### Files

| Purpose | macOS | Linux | Windows |
| --- | --- | --- | --- |
| Runtime (endpoint record, locks, maintenance marker) | `~/Library/Application Support/purlview/runtime/` | `$XDG_RUNTIME_DIR/purlview/`, else `~/.local/state/purlview/runtime/` | `%LOCALAPPDATA%\purlview\runtime\` |
| Endpoint | Unix socket `daemon.sock` in the runtime directory when the path fits the kernel limit (103 bytes), otherwise `$TMPDIR/purlview-<uid>/<hash>.sock` | same (limit 107 bytes; fallback under `/tmp`) | named pipe `\\.\pipe\purlview-<user SID>-<hash>` |
| Log (bounded: 1 MiB, one rotation) | `~/Library/Logs/purlview/daemon.log` | `~/.local/state/purlview/logs/daemon.log` | `%LOCALAPPDATA%\purlview\logs\daemon.log` |
| Update-check cache | `~/Library/Caches/purlview/update-check.json` | `~/.cache/purlview/update-check.json` | `%LOCALAPPDATA%\purlview\cache\update-check.json` |
| Credential (private file written by `login`, read by the CLI) | `~/Library/Application Support/purlview/credentials.json` | `~/.local/state/purlview/credentials.json` | `%LOCALAPPDATA%\purlview\credentials.json` |

Directories are created with mode 0700 and files with 0600 (Windows: the
inherited per-user ACL of `%LOCALAPPDATA%`). `PURLVIEW_STATE_DIR=<dir>`
relocates all of them under one directory; credentials and runtime state are
additionally scoped by environment and a hash of the platform endpoint. Tests
use it so they never touch a developer's real daemon. Spaces and non-ASCII
characters in these paths are fine; on macOS and Linux a socket path longer
than the kernel allows moves to the short fallback shown above.

### Singleton, liveness and identity

- A running daemon holds an exclusive OS file lock (`daemon.lock`) for its
  lifetime (`flock` on macOS and Linux, `LockFileEx` on Windows). The lock
  is released by the kernel however the process ends, so a free lock proves
  there is no daemon even if an `endpoint.json` says otherwise. Starts are
  serialised with a second lock (`start.lock`); concurrent first shares
  produce one daemon.
- `endpoint.json` records the endpoint, the instance id (chosen by the
  command that started the daemon), the process identity (PID plus kernel
  start time), the executable path and its build. It is written after the
  daemon listens and removed on a clean exit. A stale record is cleaned up
  by the next start once the lock proves it stale. A process is never
  signalled because its name or PID looks like Purlview; `daemon stop
  --force` kills only a process whose PID and start time both match.
- Readiness is a handshake that returns the expected instance id. A start
  that does not become ready within **10 seconds** is killed and reported
  with the last log lines; no unowned process and no record remain.
- On next use, a daemon whose executable path, build identity, file size or
  modification time differ from the CLI's own executable is shut down and
  replaced before any new work. This is detection at the next command, not
  a watch on the file.

## Private control protocol

- Transport: Unix-domain socket (macOS, Linux) or named pipe (Windows). No
  TCP port, ever.
- Access: the socket lives in a 0700 directory and is 0600; the CLI checks
  ownership and mode before connecting, and the daemon checks the peer's
  uid after accepting. The pipe carries an explicit security descriptor
  naming the current user as owner with a single allow entry for that user
  and rejects remote clients; the CLI checks the pipe server's owner SID
  before sending anything. The pipe namespace is machine-wide, so one daemon
  serves all of a user's logon sessions; other users never reach it.
- Framing: one JSON object per line, at most **64 KiB**; unknown top-level
  fields, missing ids and oversized lines are protocol errors. Requests
  carry an id and `op`; responses carry the id, `ok`, and a result or an
  error with a stable `code`; events have no id.
- Operations: `hello` (protocol version, client build), `status`,
  `shutdown`, `subscribe`, `update.check`, the share operations
  `share.start|stop|stop_all|list` with the `share.event` event (defined in
  `internal/ipc/protocol.go`, served by the share engine), and the
  test-only `test.op.start|stop|list|revoke` (served only by a daemon
  started with `--test-resources`; otherwise they return `not_implemented`).
  The share operations carry the installation credential from the CLI to
  the daemon at each start; the daemon keeps it in memory only.
- Versioning: protocol version **3**, independent of the executable version.
  Version 2 replaced the share messages' single `recipient` with `recipients`,
  and version 3 added a share's `targets` and `no_rewrite`. A client and a
  daemon must speak the same protocol version for any share operation. Additive changes, such as the share event kinds
  `first_visitor`, `app_unresponsive` and `app_responding` and the record's
  `invites`, do not change the version: a client ignores event kinds and
  fields it does not know.
  `hello`, `status` and `shutdown` are answered for any client protocol so a
  newer CLI can identify and retire an older daemon; every other operation
  needs a matching `hello` first. Live work is never migrated between daemon
  versions.
- Concurrency and limits: any number of clients; each connection has a
  bounded outbound queue (128 events) and a 5 second write deadline. A
  subscriber that falls behind receives `subscriber.overflow` and is
  disconnected so it cannot stall the daemon. Client calls time out after
  5 seconds.

## Ownership and cancellation

The daemon owns **shares** and test-only operations under these rules:

- An **attached** operation belongs to the connection that created it
  (a foreground share's CLI). Ctrl-C, an explicit stop, or loss of that
  connection or process ends exactly that operation. Other work continues.
- A **detached** operation belongs to the daemon (`share --background`).
  Once the daemon has acknowledged ownership the creating CLI may exit.
- An operation with a deadline ends on its own (`expired`); reconnecting
  cannot extend it. An injected revocation ends it with `revoked`. Daemon
  shutdown ends everything with `daemon_shutdown`.
- Ending is idempotent: the first reason wins, cleanup runs once, and later
  stops report "already ended" for a bounded history (200 operations).
- A restarted daemon starts with no operations. Nothing is reloaded from
  disk and nothing is retried automatically.

For a stop from another device (`purlview stop`), the
platform enforces revocation at the gateway regardless of whether the daemon
is notified; the daemon's cancellation is local cleanup, and a local stop
that could not reach the platform must not be reported as a confirmed
remote revocation.

## Update check on share

- Every actual `share` (not help, version, completion or a usage error)
  asks the daemon for what it already knows (a local call answered from
  memory, no waiting on metadata) and announces a newer cached answer right
  after the share block, never before it. That call also starts the
  daemon's fetch; a foreground share additionally asks for the fresh answer
  with a **1.5 second** bound and announces it if it arrives while the share
  is running.
  Neither readiness nor command completion ever waits for metadata: a
  share that finishes first goes without, and the next share announces the
  cached answer. The daemon performs one conditional `GET` against the
  GitHub releases API of the release repository named at build time
  (`releases/latest` for a stable build; `releases?per_page=20` for a
  prerelease build, which follows prereleases too), with a **5 second**
  timeout and a **1 MiB** body limit, sending only a `User-Agent` and an
  `If-None-Match` header. The result lands in the cache with its ETag; a
  `304` costs no rate limit. The check finishes even if the CLI has already
  exited. An answered check (a release, `304` or no release) is reused for
  **24 hours**; only then does a share ask GitHub again, so a busy day costs
  one request.
- No check at all, and so no notice, when `PURLVIEW_NO_UPDATE_CHECK` or the
  `DO_NOT_TRACK` convention is set to anything but empty, `0` or `false`, or
  in CI (`CI` set likewise), where nobody reads the notice.
- A newer version (SemVer comparison; prereleases order before releases)
  prints two dim lines on **stderr**: the version, and the command that
  updates this copy the way it was installed (Homebrew, Scoop, WinGet,
  installer script, `go install`) or the release page (deb/rpm, unknown
  route). One version is announced at most once a day (recorded in the
  cache directory's `update-notice.json`), and only when both stdout and
  stderr are terminals. Stdout is untouched. Nothing is downloaded and no
  command from the metadata is executed. `purlview daemon status` shows a
  newer version the daemon already holds (`update: 0.9.1 available (…)`).
- Silent outcomes: no release yet or a private release repository (404),
  rate limiting (403/429, with a back-off until the reset time or 5
  minutes), network failure or timeout (30 second back-off), malformed or
  oversized metadata. Sharing's exit status never depends on the check.
- Builds from a checkout (`built by: source`, pseudo-versions, modified
  trees) and `devel` are not comparable with releases and never notify.

## Manual upgrades and maintenance

Upgrading Purlview ends active shares. Nothing is downloaded or installed
automatically; the user chooses the moment.

`purlview daemon stop --maintenance` stops the daemon (bounded wait, 15
seconds by default) and writes a marker that refuses new daemon starts for
at most **3 minutes** or until `purlview daemon resume`. The installer
scripts run it after downloading and verifying the release, replace the
executable, then resume. The Homebrew cask and the Scoop manifest do the
same through their hooks: the installed copy runs
`purlview daemon stop --maintenance` before it is removed (the cask's
`uninstall_preflight`, Scoop's `pre_install`), and the new copy runs
`purlview daemon resume` once it is in place (the cask's `postflight`,
Scoop's `post_install`). Routes without a hook (WinGet, `.deb` and `.rpm`,
`go install`) rely on next-use detection; [installation.md](installation.md)
lists each route. A failed replacement leaves the previous executable usable;
ended shares stay ended.

## Maintenance commands (hidden)

These are not part of the customer command tree and do not appear in help or
completion, but they are stable and safe to run by hand:

```sh
purlview daemon status [--json]        # exit 0 running, 1 not running
purlview daemon start                  # start or reuse; retires an outdated daemon
purlview daemon stop [--timeout 15s] [--maintenance] [--force]
purlview daemon resume                 # lift the maintenance marker early
purlview daemon run --instance <id>    # what start executes; Ctrl-C stops it
```

Environment knobs, for tests and diagnosis: `PURLVIEW_STATE_DIR`,
`PURLVIEW_DAEMON_START_TIMEOUT` (default 10s), `PURLVIEW_DAEMON_IDLE_EXIT`
(default 60s), `PURLVIEW_DAEMON_TEST_RESOURCES=1` (enables the test-only
operations and `daemon test-op`), `PURLVIEW_UPDATE_API_URL` and
`PURLVIEW_RELEASE_REPOSITORY` (point the checker elsewhere). Code running
inside a `go test` binary cannot use the default per-user paths at all
(`daemon.ErrUnisolatedTest`): it needs `PURLVIEW_STATE_DIR`, and to start a
daemon it needs the same directory in the process environment, which a
started daemon inherits. Released executables are not affected.

## Known limits

- Windows: a process inside a job object that forbids breakaway starts the
  daemon inside that job; if the job kills its members on close, the daemon
  ends with it. Ordinary consoles and Windows Terminal do not do this.
- Windows: a running daemon holds its executable open. The installer script
  and Scoop stop it first; WinGet has no hook that runs the old executable,
  so a WinGet upgrade while a share is active needs `purlview daemon stop`
  first. An idle daemon exits within 60 seconds.
- A cross-user access test needs a second OS user, which the tests in this
  repository do not create; it is a manual check (one user starts a daemon;
  another is refused and gets their own).
- The daemon does not verify a peer's *process*, only its user: any program
  running as the same user can control the daemon, as with any per-user
  agent.
