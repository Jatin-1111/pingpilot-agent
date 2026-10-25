#!/bin/sh
# Ping Pilott agent installer for Linux (systemd).
#
#   curl -fsSL https://<your-ping-pilott>/install.sh | sudo PINGPILOT_TOKEN=ppa_... sh
#
# What it does, in order, and stops at the first thing that is wrong:
#   1. checks it is Linux, that it is root, and that the token looks like a token
#   2. downloads the agent for this machine's CPU, and checksums.txt, from the GitHub release
#   3. refuses to go on unless the file's SHA-256 is the one in checksums.txt
#   4. makes an unprivileged `pingpilot-agent` user, puts the binary in /usr/local/bin and the token in
#      /etc/pingpilot-agent/token (readable by that user only)
#   5. installs a hardened systemd service, starts it, and runs `pingpilot-agent check` so you see it work
#
# Run it again to upgrade (the token is kept). Remove everything with:  sh install.sh --uninstall
#
# Options:   --uninstall        remove the agent, its user, its service and its token
#            --version vX.Y.Z   install that release instead of the newest
#            --api URL          the Ping Pilott address, for a self-hosted one (written to /etc/pingpilot-agent/agent.json)
#            --no-start         install, but do not enable or start the service
#            --help
# Settings:  PINGPILOT_TOKEN          the agent token (from the server's page in Ping Pilott)
#            PINGPILOT_RELEASE_BASE   download the files from here instead of GitHub (a mirror); must be https
#
# The whole script is one function called on the last line, so a download cut short cannot run half of it.

set -eu

REPO="Jatin-1111/pingpilot-agent"
SERVICE="pingpilot-agent"
SERVICE_USER="pingpilot-agent"
BIN="/usr/local/bin/pingpilot-agent"
CONF_DIR="/etc/pingpilot-agent"
UNIT="/etc/systemd/system/pingpilot-agent.service"

say() { printf '%s\n' "$*"; }
step() { printf '\n==> %s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

usage() {
    cat <<'USAGEEOF'
Ping Pilott agent installer for Linux (systemd).

  curl -fsSL https://<your-ping-pilott>/install.sh | sudo PINGPILOT_TOKEN=ppa_... sh

Options:
  --uninstall        remove the agent, its user, its service and its token
  --version vX.Y.Z   install that release instead of the newest
  --api URL          the Ping Pilott address, for a self-hosted one
  --no-start         install, but do not enable or start the service
  --help

Settings:
  PINGPILOT_TOKEN          the agent token (from the server's page in Ping Pilott)
  PINGPILOT_RELEASE_BASE   download the files from here instead of GitHub (a mirror); must be https

Run it again to upgrade (the token is kept).
USAGEEOF
}

have() { command -v "$1" >/dev/null 2>&1; }

# fetch URL FILE: download over https (a mirror over http only when PINGPILOT_ALLOW_HTTP=1, which is for tests)
fetch() {
    url="$1"
    out="$2"
    case "$url" in
        https://*) ;;
        http://*) [ "${PINGPILOT_ALLOW_HTTP:-}" = "1" ] || die "refusing to download over plain http: $url" ;;
        *) die "not a web address: $url" ;;
    esac
    if have curl; then
        if [ "${PINGPILOT_ALLOW_HTTP:-}" = "1" ]; then
            curl -fsSL --retry 3 --connect-timeout 15 -o "$out" "$url" || return 1
        else
            curl -fsSL --proto '=https' --tlsv1.2 --retry 3 --connect-timeout 15 -o "$out" "$url" || return 1
        fi
    elif have wget; then
        wget -q -O "$out" "$url" || return 1
    else
        die "need curl or wget to download the agent"
    fi
}

sha256_of() {
    if have sha256sum; then
        sha256sum "$1" | awk '{print $1}'
    elif have shasum; then
        shasum -a 256 "$1" | awk '{print $1}'
    elif have openssl; then
        openssl dgst -sha256 "$1" | awk '{print $NF}'
    else
        die "need sha256sum, shasum or openssl to check the download"
    fi
}

detect_arch() {
    case "$(uname -m)" in
        x86_64 | amd64) echo amd64 ;;
        aarch64 | arm64) echo arm64 ;;
        armv7l | armv7 | armv6l | armhf) echo arm ;;
        *) die "this CPU ($(uname -m)) is not supported yet. Supported: x86_64, arm64, arm (32 bit)" ;;
    esac
}

nologin_shell() {
    for shell in /usr/sbin/nologin /sbin/nologin /usr/bin/nologin /bin/false; do
        [ -x "$shell" ] && { echo "$shell"; return; }
    done
    echo /bin/false
}

make_user() {
    # The group first, so that `install -g` has one to name whichever tool makes the user: busybox's adduser would
    # otherwise put the user in "nogroup".
    if ! grep -q "^${SERVICE_USER}:" /etc/group 2>/dev/null; then
        if have groupadd; then
            groupadd --system "$SERVICE_USER"
        elif have addgroup; then
            addgroup -S "$SERVICE_USER"
        else
            die "cannot create the $SERVICE_USER group: neither groupadd nor addgroup was found"
        fi
    fi
    if id "$SERVICE_USER" >/dev/null 2>&1; then
        return
    fi
    if have useradd; then
        useradd --system --no-create-home --home-dir /nonexistent --shell "$(nologin_shell)" -g "$SERVICE_USER" "$SERVICE_USER"
    elif have adduser; then
        # Alpine / busybox
        adduser -S -H -D -G "$SERVICE_USER" -h /nonexistent -s "$(nologin_shell)" "$SERVICE_USER"
    else
        die "cannot create the $SERVICE_USER user: neither useradd nor adduser was found"
    fi
}

use_systemd() {
    have systemctl && [ -d /run/systemd/system ]
}

write_unit() {
    cat >"$UNIT" <<UNITEOF
[Unit]
Description=Ping Pilott host agent
Documentation=https://github.com/$REPO
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=$SERVICE_USER
Group=$SERVICE_USER
ExecStart=$BIN run
Restart=always
RestartSec=10

# The agent reads the machine and talks out over https. It needs nothing else, so it is given nothing else.
NoNewPrivileges=true
CapabilityBoundingSet=
AmbientCapabilities=
ProtectSystem=strict
ProtectHome=read-only
PrivateDevices=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectKernelLogs=true
ProtectControlGroups=true
ProtectClock=true
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
RestrictNamespaces=true
RestrictRealtime=true
RestrictSUIDSGID=true
LockPersonality=true
MemoryDenyWriteExecute=true
SystemCallArchitectures=native
UMask=0077

[Install]
WantedBy=multi-user.target
UNITEOF
    chmod 0644 "$UNIT"
}

valid_token() {
    printf '%s' "$1" | grep -Eq '^ppa_[0-9a-f]{24}_[A-Za-z0-9_-]{43}$'
}

do_uninstall() {
    step "Removing the Ping Pilott agent"
    if use_systemd; then
        systemctl stop "$SERVICE" 2>/dev/null || true
        systemctl disable "$SERVICE" 2>/dev/null || true
    fi
    rm -f "$UNIT"
    if use_systemd; then
        systemctl daemon-reload 2>/dev/null || true
    fi
    rm -f "$BIN"
    rm -rf "$CONF_DIR"
    if id "$SERVICE_USER" >/dev/null 2>&1; then
        if have userdel; then
            userdel "$SERVICE_USER" 2>/dev/null || true
        elif have deluser; then
            deluser "$SERVICE_USER" 2>/dev/null || true
        fi
    fi
    if grep -q "^${SERVICE_USER}:" /etc/group 2>/dev/null; then
        if have groupdel; then
            groupdel "$SERVICE_USER" 2>/dev/null || true
        elif have delgroup; then
            delgroup "$SERVICE_USER" 2>/dev/null || true
        fi
    fi
    say "Removed: the service, the program, the token and the $SERVICE_USER user and group."
    say "To stop seeing this server's host metrics in Ping Pilott, turn it off on the server's page (that also deletes its data)."
}

main() {
    uninstall=0
    version=""
    api=""
    start=1
    while [ $# -gt 0 ]; do
        case "$1" in
            --uninstall) uninstall=1 ;;
            --version) [ $# -ge 2 ] || die "--version needs a value, for example v0.1.0"; version="$2"; shift ;;
            --api) [ $# -ge 2 ] || die "--api needs a value"; api="$2"; shift ;;
            --no-start) start=0 ;;
            -h | --help) usage; exit 0 ;;
            *) die "unknown option: $1 (try --help)" ;;
        esac
        shift
    done

    [ "$(uname -s)" = "Linux" ] || die "this installer is for Linux. On macOS or Windows download the agent from https://github.com/$REPO/releases and run it as a service; see the README."
    [ "$(id -u)" -eq 0 ] || die "run it as root: put sudo before sh, for example  curl -fsSL <address>/install.sh | sudo PINGPILOT_TOKEN=... sh"

    if [ "$uninstall" -eq 1 ]; then
        do_uninstall
        return
    fi

    # the version must be a plain tag, since it goes into a web address
    if [ -n "$version" ]; then
        printf '%s' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+([-.][0-9A-Za-z.]+)?$' || die "--version must look like v0.1.0, got: $version"
    fi
    if [ -n "$api" ]; then
        printf '%s' "$api" | grep -Eq '^https://[A-Za-z0-9._:-]+(/[A-Za-z0-9._~/-]*)?$' || die "--api must be an https address with no spaces or quotes, got: $api"
    fi

    step "Checking the token"
    token="${PINGPILOT_TOKEN:-}"
    if [ -z "$token" ] && [ -r "$CONF_DIR/token" ]; then
        say "No new token given: keeping the one already installed (this is an upgrade)."
    elif [ -z "$token" ]; then
        die "no token. Copy the install command from the server's page in Ping Pilott (Host metrics), which includes it. Or set PINGPILOT_TOKEN=ppa_..."
    elif ! valid_token "$token"; then
        die "that does not look like a Ping Pilott agent token (it starts with ppa_ and is 72 characters). Copy it again from the server's page."
    else
        say "Token looks right."
    fi

    arch="$(detect_arch)"
    asset="pingpilot-agent_linux_${arch}"

    base="${PINGPILOT_RELEASE_BASE:-}"
    if [ -z "$base" ]; then
        if [ -n "$version" ]; then
            base="https://github.com/$REPO/releases/download/$version"
        else
            base="https://github.com/$REPO/releases/latest/download"
        fi
    fi
    base="${base%/}"

    tmp="$(mktemp -d)"
    trap 'rm -rf "$tmp"' EXIT INT TERM

    step "Downloading the agent for linux/$arch"
    fetch "$base/$asset" "$tmp/$asset" || die "could not download $base/$asset. Check this machine can reach github.com over https, and that the version exists."
    fetch "$base/checksums.txt" "$tmp/checksums.txt" || die "could not download $base/checksums.txt"

    step "Checking the download"
    expected="$(awk -v f="$asset" '$2 == f || $2 == ("*" f) { print $1; exit }' "$tmp/checksums.txt")"
    [ -n "$expected" ] || die "checksums.txt has no entry for $asset"
    actual="$(sha256_of "$tmp/$asset")"
    if [ "$expected" != "$actual" ]; then
        die "the downloaded file does not match its checksum (expected $expected, got $actual). Nothing was installed."
    fi
    say "Checksum matches."

    chmod 0755 "$tmp/$asset"
    # catches a binary this machine cannot run (the wrong CPU, a very old kernel) before anything is changed
    "$tmp/$asset" version >/dev/null 2>&1 || die "the downloaded agent does not run on this machine"
    say "$("$tmp/$asset" version)"

    step "Installing"
    make_user
    install -d -m 0755 "$(dirname "$BIN")"
    install -m 0755 -o root -g root "$tmp/$asset" "$BIN.new"
    mv -f "$BIN.new" "$BIN"

    install -d -m 0755 "$CONF_DIR"
    if [ -n "$token" ]; then
        # written by a process with a tight umask, then given to the service user: never readable by anyone else
        ( umask 077; printf '%s\n' "$token" >"$tmp/token" )
        install -m 0600 -o "$SERVICE_USER" -g "$SERVICE_USER" "$tmp/token" "$CONF_DIR/token"
    fi
    if [ -n "$api" ]; then
        printf '{\n  "api": "%s"\n}\n' "$api" >"$tmp/agent.json"
        install -m 0644 -o root -g root "$tmp/agent.json" "$CONF_DIR/agent.json"
    fi
    say "Program: $BIN"
    say "Token:   $CONF_DIR/token (readable by $SERVICE_USER only)"

    if ! use_systemd; then
        warn "this machine does not use systemd, so no service was set up."
        say ""
        say "The agent is installed. Run it under your own service manager as the $SERVICE_USER user:"
        say "    $BIN run"
        say "and test it with:  su -s /bin/sh $SERVICE_USER -c '$BIN check'"
        say "(On a host that runs containers you can use the Docker image instead; see the README.)"
        return
    fi

    write_unit
    systemctl daemon-reload
    if [ "$start" -eq 0 ]; then
        say "Service installed but not started (--no-start). Start it with:  systemctl enable --now $SERVICE"
        return
    fi
    systemctl enable "$SERVICE" >/dev/null 2>&1
    # restart, not start: an upgrade has to replace the running agent
    systemctl restart "$SERVICE"
    say "Service started and set to start at boot."

    step "Checking it works"
    sleep 2
    if have runuser; then
        runuser -u "$SERVICE_USER" -- "$BIN" check || warn "the check found a problem: read the lines marked FAIL above."
    else
        su -s /bin/sh "$SERVICE_USER" -c "$BIN check" || warn "the check found a problem: read the lines marked FAIL above."
    fi

    say ""
    say "Done. The server's page in Ping Pilott shows its first reading within a minute or two."
    say "Logs:       journalctl -u $SERVICE -f"
    say "Test again: runuser -u $SERVICE_USER -- $BIN check"
    say "Remove it:  curl -fsSL <address>/install.sh | sudo sh -s -- --uninstall"
}

main "$@"
