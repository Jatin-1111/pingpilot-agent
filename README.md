# Ping Pilott agent

A small program that reports a server's CPU, memory, disk and network to [Ping Pilott](https://pingpilott.com) once a minute, so that when a check fails you can see whether the machine was out of memory, disk or CPU.

One static binary (6–7 MB). It needs nothing installed on the server. It opens no port: it only makes outbound HTTPS requests.

## Install

On the server's page in Ping Pilott, turn on **Host metrics**. It shows the exact command with your token in it. You do not need to build or configure anything.

**A Linux server** (systemd):

```
curl -fsSL https://<your-ping-pilott>/install.sh | sudo PINGPILOT_TOKEN=ppa_... sh
```

It downloads the agent for the machine's CPU (x86-64, arm64 or 32-bit arm), **refuses to go on unless the file matches its published checksum**, creates an unprivileged `pingpilot-agent` user, keeps the token readable by that user only, installs a hardened systemd service, starts it, and runs `pingpilot-agent check` so you see it work. Run it again to upgrade (the token is kept). Remove everything with:

```
curl -fsSL https://<your-ping-pilott>/install.sh | sudo sh -s -- --uninstall
```

Prefer to read it first? Download it, read it, then run it: `curl -fsSLO https://<your-ping-pilott>/install.sh`, then `sudo PINGPILOT_TOKEN=ppa_... sh install.sh`.

**A host that runs containers:**

```
docker run -d --name pingpilot-agent --restart unless-stopped \
  --pid=host --net=host --uts=host -v /:/host:ro \
  -e PINGPILOT_HOST_ROOT=/host -e PINGPILOT_TOKEN=ppa_... \
  ghcr.io/jatin-1111/pingpilot-agent
```

The agent reports on the machine, not on its own container, so it is given the machine: `--pid=host` (the processes, by name), `--net=host` (the real network interfaces), `--uts=host` (the host's name) and the root filesystem, read only, to measure its disks.

**Anywhere else** (macOS, Windows, a Linux without systemd): download `pingpilot-agent_<os>_<arch>` and `checksums.txt` from the [latest release](https://github.com/Jatin-1111/pingpilot-agent/releases/latest), check the file against `checksums.txt` (`sha256sum -c`), and run `pingpilot-agent run` under your own service manager. `pingpilot-agent check` tells you if it can reach Ping Pilott.

## What it sends

Every 60 seconds, one snapshot:

| | |
|---|---|
| CPU | percent busy, number of cores, load average (Linux and macOS) |
| Memory | total and used (not counting cache), swap |
| Disks | total and used for each real disk (not tmpfs, containers or snaps), at most 16 |
| Network | bytes per second in and out, over the machine's own interfaces |
| Uptime | seconds |
| Processes | the 5 busiest **by name**, with CPU and memory percent |

## What it never does

- It never sends a command line, a file, an environment variable or a path other than a disk's mount point. Process **names only**: command lines are where passwords and keys end up.
- It never accepts a command. The only thing it receives is "thanks, send the next one in 60 seconds".
- It never opens a port, writes to disk, or needs root. Run it as an unprivileged user.
- It never follows a redirect, and it sends the token over plain `http` only to `localhost`.

## Commands

```
pingpilot-agent            report until stopped (what the service runs)
pingpilot-agent check      test the token, the network and the reading, and say what to fix
pingpilot-agent print      take one reading and print what would be sent, without sending it
pingpilot-agent version
```

`check` is the one to run when something does not work. It walks the same road a snapshot takes and stops at the step that fails:

```
Token
  [ ok ] found in /etc/pingpilot-agent/token (ppa_674d...)
Network
  [ ok ] https://ping-pilott-backend.onrender.com reachable (HTTP 200 in 180ms)
Sign-in
  [ ok ] host read: 4 cores, CPU 13%, memory 3.2 GiB of 7.8 GiB, 2 disks, 5 processes listed
  [ ok ] Ping Pilott accepted the reading (1 stored)
```

## Settings

The token is the only secret. It is read from a file or the environment, never from a flag, because a flag shows in the process list for every user on the machine.

| Setting | Where | Default |
|---|---|---|
| Token | `PINGPILOT_TOKEN`, or the file named by `--token-file` / `PINGPILOT_TOKEN_FILE` | `/etc/pingpilot-agent/token` (Windows: `%ProgramData%\pingpilot-agent\token`) |
| Address | `--api`, `PINGPILOT_API`, or `"api"` in the config file | `https://ping-pilott-backend.onrender.com` |
| Config file | `--config`, `PINGPILOT_CONFIG` | `/etc/pingpilot-agent/agent.json` |

Flags win over the environment, which wins over the file. The address must be `https`, except for `localhost`.

Keep the token file readable by the agent's user only (`chmod 600`); `check` warns when it is not.

## When the network or the token fails

- **Network down or Ping Pilott unreachable:** the last 10 readings (and nothing older than 14 minutes) are kept and sent together, in order, when it is back. Retries start at 5 seconds and slow down to the interval. Failures are logged when they start and now and then, not every minute.
- **Token refused (401):** it stops reading, says so once, and tries again after 5 minutes, doubling up to an hour. When you put a new token in the file, it picks it up on the next try.
- **Plan without host metrics (403):** it says so once and asks again every 15 minutes.
- **Too fast (429):** it waits for the time the server asks.

Agents that start together (a fleet restart, an outage that ends) are spread by random jitter, so they do not all arrive in the same second.

## Build

```
go build -trimpath -ldflags "-s -w -X main.version=0.1.0" -o pingpilot-agent ./cmd/pingpilot-agent
go test ./...
```

Cross-compile with `GOOS` and `GOARCH`; there is no C code (`CGO_ENABLED=0`).

The format of what is sent is defined by the API (`utils/hostMetrics.js` in the backend), which refuses unknown fields and out-of-range numbers. `internal/snapshot` makes whatever is measured fit those rules.

## Releases

Push a tag and the release builds itself:

```
git tag v0.1.0 && git push origin v0.1.0
```

`.github/workflows/release.yml` builds every platform with GoReleaser, publishes the binaries, `checksums.txt` and `install.sh` to the GitHub release (the binaries carry no version in their names, so `releases/latest/download/<name>` always means "the newest"), signs their build provenance (`gh attestation verify <file> --repo Jatin-1111/pingpilot-agent`), and pushes the container image to `ghcr.io/jatin-1111/pingpilot-agent` for amd64 and arm64.

`install.sh` is also kept in the Ping Pilott backend (`assets/install.sh`), which serves it at `/install.sh`. When it changes here, copy it there; the backend's tests compare the two when both repositories are checked out side by side.

## Testing the installer

`sh scripts/test-installer.sh` runs the real `install.sh` in fresh Debian and Alpine containers, piped into `sh` as a user would: install, upgrade, uninstall, and every way it should refuse (no token, bad token, not root, a download that does not match its checksum, plain http). systemd does not run in a container, so `systemctl` is a stub that records its calls, and the unit the installer writes is checked by the real `systemd-analyze verify`. Needs Docker, Go, Python and `sha256sum`.
