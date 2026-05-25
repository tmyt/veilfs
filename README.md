# veilfs

A FUSE filesystem that hides files from your AI coding agent's view.

veilfs re-presents a source directory while hiding files matching a
`.veilignore` policy. Inside the mount, ignored files don't appear in
listings, can't be opened, and can't be overwritten by entries of the
same name. Built for the workflow where you run an AI coding agent inside
Docker and want a kernel-enforced boundary between the agent and a
specific subset of files in its workspace.

## What it solves

When an AI coding agent has shell access to your project, anything it
reads, lists, or echoes can end up in its context window — and from
there can flow to logs, search queries, telemetry, and tool calls.
Application-level ignore files (`.claudeignore`, `.cursorignore`) help,
but they only restrict the agent's own file tools. The moment the agent
runs `cat .env`, `printenv`, or a build script that prints loaded
environment variables, those ignores no longer apply.

veilfs closes that gap by removing the relevant files from the agent's
filesystem entirely. They are not denied. They don't exist.

## Why FUSE

The hiding happens at the filesystem layer, so it applies to every
process inside the mount automatically — the agent, subprocesses it
spawns, MCP servers, debug shells. Application-level ignore files only
restrict the agent's own file tools; veilfs doesn't have that gap.

`.veilignore` uses gitignore syntax. Files that match are reported as
`ENOENT` on lookup; writes that would create a hidden name are refused
with `EPERM`. The policy hot-reloads when `.veilignore` changes — no
remount required.

## Quick start

### Local

```bash
go build -o veilfs .

mkdir -p /tmp/src /tmp/veil
echo "topsecret" > /tmp/src/.env
echo "ok"        > /tmp/src/main.go
printf ".env\n"  > /tmp/src/.veilignore

./veilfs mount /tmp/src /tmp/veil
ls /tmp/veil                  # main.go only — .env and .veilignore are hidden
cat /tmp/veil/.env            # No such file or directory
echo HIJACK > /tmp/veil/.env  # Operation not permitted

./veilfs umount /tmp/veil
```

### Linux (`veilfs run`)

On Linux, use `veilfs run` to launch a command (or a shell) with a veiled
view of the current directory. No mount setup, no Docker required.

```bash
echo "topsecret" > .env
echo "ok"        > main.go
printf ".env\n"  > .veilignore

veilfs run . -- bash
# Inside this shell, .env and .veilignore don't exist.
# Spawned processes inherit the veiled view automatically.
exit
```

`veilfs run .` (no command) drops you into your `$SHELL`. Any command
works: `veilfs run . -- claude`, `veilfs run . -- pnpm test`,
`veilfs run . -- python script.py`.

Uses Linux mount namespaces. Requires unprivileged user namespaces (default
enabled on most distros). Not available on macOS — use the Docker path
below.

### Docker

```bash
docker run --rm -it \
    --device /dev/fuse \
    --cap-add SYS_ADMIN \
    --security-opt apparmor=unconfined \
    -v "$(pwd):/work-source" \
    your-agent-image
```

Inside the container:

```bash
veilfs mount /work-source /work
cd /work
# Run your agent here.
```

A full working demo with sample policy and `.env` is in
[`examples/docker-demo/`](examples/docker-demo/).

### macOS

Requires [macFUSE](https://osxfuse.github.io/) (`brew install --cask macfuse`
plus a reboot to approve the kernel extension). After that, `go build`
and use the binary the same way.

## `.veilignore` syntax

Standard gitignore syntax. `.veilignore` itself is always hidden from the
mount, even if not listed in the policy. Other highlights:

```
# Hide a specific filename anywhere in the tree.
secret.env

# Hide only at the source root.
/secret.env

# Hide a directory (and everything inside).
secrets/

# Hide by pattern.
*.pem
**/credentials.json

# Re-expose a file that an earlier rule would have hidden.
!important.log
```

Editing the file (or the file passed via `--config`) takes effect on the
next syscall — there is no cache to flush.

## CLI

```
veilfs mount [flags] <source> <target>
veilfs umount <target>
veilfs run [flags] [<source>] [-- <command> [args...]]
```

| Flag | Default | Effect | Commands |
|------|---------|--------|----------|
| `--config FILE` | `<source>/.veilignore` | Use FILE as the ignore policy instead of the auto-located `.veilignore`. | mount, run |
| `-f` | off | Foreground mode for debugging. Without it the process daemonizes. | mount |
| `--debug` | off | Verbose FUSE protocol logging on stderr. | mount, run |
| `--case-mode {auto,on,off}` | `auto` | Case-folding for pattern matching. `auto` probes the source filesystem; fails closed (case-insensitive) if the probe can't decide. | mount, run |
| `--cache-timeout <duration>` | `0` | Kernel cache lifetime for dentry and attr. A bare number means seconds (`2`, `0.5`); a Go duration suffix also works (`1s`, `500ms`). Non-zero values trade policy reload latency for syscall throughput. | mount, run |
| `--keep-cwd` | off | Run the command in the caller's current directory instead of chdir-ing into the veiled source root. | run |

`mount`'s `<source>` and `<target>` must both exist as directories. They
may not nest (rejected pre-mount). Symlinks in either argument are
canonicalized at mount time.

`run`'s `<source>` defaults to the current directory; the command after
`--` defaults to `$SHELL`. It is Linux-only — see the
[`veilfs run`](#linux-veilfs-run) section above.

## What it doesn't do

- It does not defend against a process that can read the underlying
  source tree directly (outside the mount).
- It does not defend against `/proc/self/mountinfo` walks by a process
  with `CAP_SYS_ADMIN`.
- It does not prevent network exfiltration, prompt injection, or any
  other leak channel that does not go through direct file reads.
- It is not a substitute for proper secrets management. Production
  credentials should not live on disk in plaintext in the first place.

## Related work

### Same approach, different tradeoffs

- **[ai-sandbox-dkmcp](https://github.com/YujiSuzuki/ai-sandbox-dkmcp)** —
  Hides specific files by mounting `/dev/null` or `tmpfs` over them via
  Docker volumes. Simpler setup; no pattern matching, no hot reload.
- **[landrun](https://github.com/Zouuup/landrun)** —
  Sandboxes commands using Linux Landlock LSM with per-path access controls.
  Permission-gating semantics (`EACCES` on access) rather than existence
  hiding; path-based rather than pattern-based.
- **[YoloFS](https://arxiv.org/abs/2604.13536)** — Research filesystem
  for agent safety with staging, snapshots, and progressive permission.
  Broader scope; currently a paper, not a shipped tool.

### Different layer, complementary

- **[Claude Code sandboxing](https://code.claude.com/docs/en/sandboxing)** —
  OS-level process isolation (Seatbelt on macOS, bubblewrap on Linux).
  Restricts what processes can do; veilfs restricts what files they see.
- **[Doppler](https://doppler.com/) / [Bitwarden Secrets Manager](https://bitwarden.com/products/secrets-manager/)** —
  Runtime secret injection so secrets never land on disk. Different
  paradigm; can coexist with veilfs.

## Contributing

Issues and PRs welcome. The project is experimental; no formal process.

## License

MIT
