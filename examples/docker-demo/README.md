# veilfs Docker demo

A self-contained sandbox to experience what `veilfs` does. A throwaway
sample project on the host is bind-mounted into a Linux container; the
container's entrypoint then applies `veilfs` between the bind mount and
the shell you land in. The host directory stays untouched — you can edit
`.veilignore` from outside and watch the in-container view react.

## Layout

```
examples/docker-demo/
├── Dockerfile                # multi-stage: compiles veilfs, then ships fuse3 + binary
├── veil-entrypoint.sh        # mounts veilfs and execs the user command
├── run.sh                    # builds the image and drops you into the veiled shell
└── sample-project/           # what gets mounted into /work-source
    ├── .veilignore
    ├── .env                  # hidden by .veilignore
    ├── secrets/
    │   ├── api-key.pem       # hidden via "secrets/"
    │   └── db-password.txt
    ├── main.go               # visible
    ├── README.md             # visible
    ├── logs/normal.log       # visible
    └── node_modules/         # visible: .veilignore is NOT .gitignore,
        └── some-pkg/...      # so node_modules is left alone for the agent
```

## Run it

```sh
./run.sh
```

The script will:

1. Build the image `veilfs-demo:local` (multi-stage; compiles `veilfs`
   from this repo's source).
2. `docker run` it with `--device /dev/fuse --cap-add SYS_ADMIN` and your
   `sample-project/` bind-mounted at `/work-source`.
3. The entrypoint runs `veilfs mount /work-source /work` and execs
   `bash` with cwd set to `/work`.

To target a different host directory:

```sh
./run.sh /path/to/your/project
```

## Try inside the container

```sh
ls                          # only main.go, README.md, logs/, node_modules/
cat README.md               # visible
cat .env                    # "No such file or directory"
ls secrets/                 # "No such file or directory"
echo HIJACK > .env          # "Operation not permitted"  (write protection)
echo HIJACK > new.txt       # ok — non-matching name writes through
ls /work-source             # empty — the original bind mount has been
                            #   re-bound to a randomized internal path and
                            #   the well-known /work-source mount removed
                            #   so this trivial bypass closes.
```

`/work` is the veiled view. The demo `bash` lands you in `/work`.

The entrypoint moves the raw bind mount to a randomized path under
`/tmp/.veilfs-…` and removes the visible `/work-source` mount, so a
curious agent cannot just `cat /work-source/.env` to bypass the filter.
A root process inside the container could still find the internal path
via `/proc/self/mountinfo`; for hardened isolation pair this demo with
a non-root user that does not have CAP_SYS_ADMIN.

## Try the hot reload (from another host terminal)

```sh
# In another terminal on the host:
echo "main.go" >> examples/docker-demo/sample-project/.veilignore

# Back in the container shell, almost immediately:
ls    # main.go is gone
```

Reload latency is bounded by `fsnotify` delivery (single-digit
milliseconds in normal conditions). Both `EntryTimeout` and
`AttrTimeout` are pinned to zero in `internal/vfs/veilfs.go` so the
kernel never serves a stale dentry or attribute snapshot for a
hidden path.

## Cleanup

Exit the container with `exit` or Ctrl-D. The kernel auto-unmounts the
FUSE filesystem when the container's mount namespace is destroyed. The
host's `sample-project/` is unchanged (except for whatever you wrote to
it on purpose).

## Caveats

- **macOS / Docker Desktop**: everything works because the veiled view
  is only consumed inside the container. The view is *not* propagated
  back to the host's macOS filesystem (shared mount propagation cannot
  cross the Docker Desktop VM boundary).
- **SELinux hosts**: replace `--security-opt apparmor=unconfined` with
  `--security-opt label=disable` in `run.sh`.
- **No `-it`**: if you wrap a non-interactive command instead of `bash`,
  drop `-it` accordingly.
