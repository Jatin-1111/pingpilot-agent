#!/bin/sh
# Tests install.sh on real Linux, in containers, the way a user runs it (piped into sh).
#
# It builds two fake releases of the agent (one good, one whose binary does not match its checksum), serves them from this
# machine, and runs the installer against them in fresh Debian and Alpine containers: a fresh install, an upgrade, an
# uninstall, and every way it should refuse. systemd itself does not run in a container, so `systemctl` is a stub that
# records what it was asked to do, and the unit file the installer writes is checked by the real `systemd-analyze verify`.
#
# Needs: docker, go, python3, sha256sum. Run from anywhere:  sh scripts/test-installer.sh
set -eu

ROOT=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d)
PORT=${INSTALLER_TEST_PORT:-18765}
PASS=0
FAIL=0
SERVER_PID=""

cleanup() {
    [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null || true
    rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

pass() { PASS=$((PASS + 1)); printf '  ok    %s\n' "$1"; }
fail() { FAIL=$((FAIL + 1)); printf '  FAIL  %s\n' "$1"; [ $# -lt 2 ] || printf '%s\n' "$2" | sed 's/^/          /'; }

# the docker volume path of a directory (on Windows under Git Bash it must be the Windows path)
winpath() { if pwd -W >/dev/null 2>&1; then (cd "$1" && pwd -W); else (cd "$1" && pwd); fi; }

echo "building two fake releases"
mkdir -p "$WORK/good" "$WORK/bad"
for arch in amd64 arm64; do
    (cd "$ROOT" && GOOS=linux GOARCH=$arch CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=9.9.9" -o "$WORK/good/pingpilot-agent_linux_$arch" ./cmd/pingpilot-agent)
done
(cd "$WORK/good" && sha256sum pingpilot-agent_linux_* > checksums.txt)
cp "$WORK/good/"* "$WORK/bad/"
for f in "$WORK/bad/pingpilot-agent_linux_"*; do printf 'tampered' >> "$f"; done # the checksums.txt is the good one

# python3, or python where that is its only name (on Windows python3 can be a Store shortcut that does not run)
PY=""
for candidate in python3 python; do
    if "$candidate" -c "import http.server" >/dev/null 2>&1; then PY=$candidate; break; fi
done
[ -n "$PY" ] || { echo "need python3 to serve the fake release"; exit 1; }
"$PY" -m http.server "$PORT" --bind 0.0.0.0 --directory "$WORK" >/dev/null 2>&1 &
SERVER_PID=$!
sleep 1

echo "building the test images"
cat > "$WORK/stub-systemctl" <<'EOF'
#!/bin/sh
echo "$*" >> /tmp/systemctl.log
exit 0
EOF
cat > "$WORK/Dockerfile.debian" <<'EOF'
FROM debian:12-slim
RUN apt-get update -qq && apt-get install -y -qq --no-install-recommends curl ca-certificates systemd procps >/dev/null \
 && mkdir -p /run/systemd/system && rm -rf /var/lib/apt/lists/*
COPY stub-systemctl /usr/local/bin/systemctl
RUN chmod +x /usr/local/bin/systemctl
EOF
cat > "$WORK/Dockerfile.alpine" <<'EOF'
FROM alpine:3.20
EOF
docker build -q -f "$WORK/Dockerfile.debian" -t pp-installer-debian "$WORK" >/dev/null
docker build -q -f "$WORK/Dockerfile.alpine" -t pp-installer-alpine "$WORK" >/dev/null

TOKEN=ppa_0123456789abcdef01234567_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnop_
BASE="http://host.docker.internal:$PORT"

# run IMAGE SCRIPT [docker options...]: the script runs in a fresh container, as root, with the installer at /install.sh.
# It always succeeds: what is checked is the output, and the scripts print the exit status they care about.
# The hosted backend's name points at nothing, so the installer's final `check` never reaches the real one.
run() {
    image="$1"; script="$2"; shift 2
    MSYS_NO_PATHCONV=1 docker run --rm -i --add-host=host.docker.internal:host-gateway --add-host=ping-pilott-backend.onrender.com:127.0.0.1 \
        -v "$(winpath "$ROOT")/install.sh:/install.sh:ro" \
        -e PINGPILOT_ALLOW_HTTP=1 -e "PINGPILOT_RELEASE_BASE=$BASE/good" "$@" "$image" sh -c "$script" 2>&1 || true
}

assert_contains() { # name output needle
    case "$2" in *"$3"*) pass "$1" ;; *) fail "$1 (expected: $3)" "$2" ;; esac
}
assert_not_contains() {
    case "$2" in *"$3"*) fail "$1 (unexpected: $3)" "$2" ;; *) pass "$1" ;; esac
}

for image in debian alpine; do
    img="pp-installer-$image"
    echo
    echo "== $image =="

    out=$(run "$img" 'cat /install.sh | sh -s -- --help; echo "exit=$?"')
    assert_contains "--help prints usage when piped" "$out" "--uninstall"

    out=$(run "$img" 'cat /install.sh | sh; echo "exit=$?"')
    assert_contains "no token is refused with what to do" "$out" "no token"
    assert_contains "no token exits non-zero" "$out" "exit=1"

    out=$(run "$img" 'cat /install.sh | PINGPILOT_TOKEN=ppa_oops sh; echo "exit=$?"; ls /usr/local/bin/pingpilot-agent 2>&1')
    assert_contains "a malformed token is refused" "$out" "does not look like a Ping Pilott agent token"
    assert_contains "and nothing is installed" "$out" "No such file"

    out=$(run "$img" 'cat /install.sh | PINGPILOT_TOKEN='"$TOKEN"' sh; echo "exit=$?"' --user 1000:1000)
    assert_contains "not root is refused" "$out" "run it as root"

    out=$(run "$img" 'cat /install.sh | PINGPILOT_TOKEN='"$TOKEN"' sh -s -- --version latest; echo "exit=$?"')
    assert_contains "a version that is not a tag is refused" "$out" "--version must look like"

    out=$(run "$img" 'cat /install.sh | PINGPILOT_TOKEN='"$TOKEN"' sh -s -- --api "https://x.example\"; rm -rf /"; echo "exit=$?"')
    assert_contains "an address with quotes in it is refused" "$out" "--api must be an https address"

    out=$(run "$img" 'cat /install.sh | PINGPILOT_TOKEN='"$TOKEN"' PINGPILOT_RELEASE_BASE='"$BASE"'/bad sh; echo "exit=$?"; ls /usr/local/bin/pingpilot-agent 2>&1; id pingpilot-agent >/dev/null 2>&1 && echo user-exists || echo user-absent')
    assert_contains "a download that does not match its checksum is refused" "$out" "does not match its checksum"
    assert_contains "and nothing is installed" "$out" "No such file"
    assert_contains "and no user is made" "$out" "user-absent"

    out=$(run "$img" 'cat /install.sh | PINGPILOT_TOKEN='"$TOKEN"' PINGPILOT_RELEASE_BASE=http://nowhere.invalid sh; echo "exit=$?"' )
    assert_contains "an unreachable download says so" "$out" "could not download"

    out=$(run "$img" 'cat /install.sh | PINGPILOT_TOKEN='"$TOKEN"' PINGPILOT_ALLOW_HTTP= sh; echo "exit=$?"')
    assert_contains "plain http is refused unless the test switch is on" "$out" "refusing to download over plain http"

    # a full install without starting
    out=$(run "$img" '
        cat /install.sh | PINGPILOT_TOKEN='"$TOKEN"' sh -s -- --no-start --api https://pp.example.com/base || exit 1
        echo "---"
        ls -l /usr/local/bin/pingpilot-agent
        stat -c "%a %U:%G" /etc/pingpilot-agent/token
        stat -c "%a" /etc/pingpilot-agent
        grep -c "^ppa_" /etc/pingpilot-agent/token
        cat /etc/pingpilot-agent/agent.json
        getent passwd pingpilot-agent
        /usr/local/bin/pingpilot-agent version
        ls /etc/systemd/system/pingpilot-agent.service 2>&1
    ')
    assert_contains "installs the program" "$out" "/usr/local/bin/pingpilot-agent"
    assert_contains "the token is readable by the agent's user only" "$out" "600 pingpilot-agent:pingpilot-agent"
    assert_contains "the agent runs and is the version served" "$out" "pingpilot-agent 9.9.9 (linux/"
    assert_contains "a service user without a login shell exists" "$out" "pingpilot-agent:"
    assert_contains "the user cannot log in" "$out" "nologin"
    assert_contains "the address was written as JSON" "$out" '"api": "https://pp.example.com/base"'
    assert_contains "the checksum was checked" "$out" "Checksum matches"

    case "$image" in
    debian)
        # install with the (stub) service manager, then check the unit with the real systemd-analyze
        out=$(run "$img" '
            cat /install.sh | PINGPILOT_TOKEN='"$TOKEN"' sh || exit 1
            echo "--- systemctl calls"
            cat /tmp/systemctl.log
            echo "--- unit"
            cat /etc/systemd/system/pingpilot-agent.service
            echo "--- verify"
            systemd-analyze verify /etc/systemd/system/pingpilot-agent.service 2>&1; echo "verify-exit=$?"
        ')
        assert_contains "the service is enabled" "$out" "enable pingpilot-agent"
        assert_contains "and restarted (so an upgrade replaces the running agent)" "$out" "restart pingpilot-agent"
        assert_contains "the unit runs as the unprivileged user" "$out" "User=pingpilot-agent"
        assert_contains "the unit is hardened" "$out" "NoNewPrivileges=true"
        assert_contains "the unit cannot gain capabilities" "$out" "CapabilityBoundingSet="
        assert_contains "systemd accepts the unit" "$out" "verify-exit=0"
        assert_not_contains "systemd found nothing unknown in it" "$out" "Unknown key"
        assert_contains "the installer ran the check at the end" "$out" "Checking it works"

        # an upgrade keeps the token and replaces the program
        out=$(run "$img" '
            cat /install.sh | PINGPILOT_TOKEN='"$TOKEN"' sh >/dev/null || exit 1
            before=$(cat /etc/pingpilot-agent/token)
            echo old > /usr/local/bin/pingpilot-agent
            cat /install.sh | sh 2>&1 | grep -E "keeping|Checksum|Service started"
            after=$(cat /etc/pingpilot-agent/token)
            [ "$before" = "$after" ] && echo token-kept
            /usr/local/bin/pingpilot-agent version
        ')
        assert_contains "an upgrade without a token keeps the one installed" "$out" "keeping the one already installed"
        assert_contains "and really keeps it" "$out" "token-kept"
        assert_contains "and replaces the program" "$out" "pingpilot-agent 9.9.9"

        # uninstall removes everything
        out=$(run "$img" '
            cat /install.sh | PINGPILOT_TOKEN='"$TOKEN"' sh >/dev/null || exit 1
            cat /install.sh | sh -s -- --uninstall
            ls /usr/local/bin/pingpilot-agent /etc/pingpilot-agent /etc/systemd/system/pingpilot-agent.service 2>&1
            id pingpilot-agent >/dev/null 2>&1 && echo user-exists || echo user-absent
            grep -c "^pingpilot-agent:" /etc/group || true
            cat /tmp/systemctl.log
        ')
        assert_contains "uninstall removes the program, the token and the unit" "$out" "No such file"
        assert_not_contains "the program is gone" "$out" "/usr/local/bin/pingpilot-agent: "
        assert_contains "uninstall removes the user" "$out" "user-absent"
        assert_contains "uninstall stops and disables the service" "$out" "disable pingpilot-agent"
        assert_contains "uninstall reminds how to turn it off in Ping Pilott" "$out" "turn it off on the server's page"
        ;;
    alpine)
        out=$(run "$img" 'cat /install.sh | PINGPILOT_TOKEN='"$TOKEN"' sh; echo "exit=$?"; id pingpilot-agent; ls /usr/local/bin/pingpilot-agent')
        assert_contains "with no systemd it installs and says how to run the agent" "$out" "does not use systemd"
        assert_contains "and the user was made with adduser" "$out" "uid="
        ;;
    esac
done

echo
echo "$PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
