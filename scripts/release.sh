#!/bin/sh
# Make a release from this machine: test, build every platform, checksum, publish to GitHub.
#
#   sh scripts/release.sh v0.1.0 --dry-run     build and verify everything, publish nothing
#   sh scripts/release.sh v0.1.0               the same, then create the GitHub release (and its tag)
#
# What it publishes is what scripts/../install.sh downloads: one binary per platform named
# pingpilot-agent_<os>_<arch>[.exe], checksums.txt (SHA-256 of each), and install.sh itself. Nothing is published unless
# the tests pass, the tree is clean and on main, the checksums verify, and the version is new.
#
# Needs: go, sha256sum, git, and (to publish) the GitHub CLI signed in: gh auth status
set -eu

ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT"

die() { printf 'error: %s\n' "$*" >&2; exit 1; }
step() { printf '\n==> %s\n' "$*"; }

VERSION="${1:-}"
DRY=0
[ "${2:-}" = "--dry-run" ] && DRY=1
printf '%s' "$VERSION" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$' || die "usage: sh scripts/release.sh vX.Y.Z [--dry-run]"
NUMBER=${VERSION#v}

for tool in go sha256sum git; do command -v "$tool" >/dev/null 2>&1 || die "need $tool"; done

step "Checking the tree"
[ "$(git rev-parse --abbrev-ref HEAD)" = "main" ] || die "releases are made from main (you are on $(git rev-parse --abbrev-ref HEAD))"
[ -z "$(git status --porcelain)" ] || die "the tree has uncommitted changes: commit them first, so the release matches a commit"
if [ "$DRY" -eq 0 ]; then
    command -v gh >/dev/null 2>&1 || die "need the GitHub CLI (gh) to publish"
    gh auth status >/dev/null 2>&1 || die "gh is not signed in: run gh auth login"
    git fetch -q origin main
    [ "$(git rev-parse HEAD)" = "$(git rev-parse origin/main)" ] || die "origin/main is not this commit: push (or pull) first, so the release is of what GitHub has"
    if gh release view "$VERSION" >/dev/null 2>&1; then die "release $VERSION already exists"; fi
fi
COMMIT=$(git rev-parse HEAD)
echo "version $VERSION at $(git rev-parse --short HEAD)"

step "Testing"
go vet ./...
go test -count=1 ./...

step "Building"
DIST="$ROOT/dist"
rm -rf "$DIST"
mkdir -p "$DIST"
# os arch [arm version]
for target in "linux amd64" "linux arm64" "linux arm 7" "darwin amd64" "darwin arm64" "windows amd64" "windows arm64"; do
    # shellcheck disable=SC2086
    set -- $target
    os=$1
    arch=$2
    ext=""
    [ "$os" = "windows" ] && ext=".exe"
    out="$DIST/pingpilot-agent_${os}_${arch}${ext}"
    GOOS=$os GOARCH=$arch GOARM=${3:-} CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$NUMBER" -o "$out" ./cmd/pingpilot-agent
    printf '  %-34s %s\n' "$(basename "$out")" "$(wc -c < "$out" | tr -d ' ') bytes"
done
cp "$ROOT/install.sh" "$DIST/install.sh"

step "Checksums"
(cd "$DIST" && sha256sum pingpilot-agent_* install.sh > checksums.txt && sha256sum -c checksums.txt)
cat "$DIST/checksums.txt"

step "Sanity: the Linux x86-64 build runs and says the right version"
if [ "$(uname -s)" = "Linux" ] && [ "$(uname -m)" = "x86_64" ]; then
    got=$("$DIST/pingpilot-agent_linux_amd64" version)
elif command -v docker >/dev/null 2>&1; then
    got=$(MSYS_NO_PATHCONV=1 docker run --rm -v "$(cd "$DIST" && (pwd -W 2>/dev/null || pwd)):/d:ro" alpine:3.20 /d/pingpilot-agent_linux_amd64 version 2>/dev/null || true)
else
    got=""
fi
case "$got" in
    "pingpilot-agent $NUMBER (linux/amd64)") echo "  $got" ;;
    "") echo "  skipped: no way to run a Linux binary here" ;;
    *) die "the build reports '$got', expected version $NUMBER" ;;
esac

if [ "$DRY" -eq 1 ]; then
    printf '\nDry run done: %s/dist holds the release. Nothing was published.\n' "$ROOT"
    exit 0
fi

step "Publishing $VERSION"
gh release create "$VERSION" "$DIST"/pingpilot-agent_* "$DIST/install.sh" "$DIST/checksums.txt" \
    --target "$COMMIT" \
    --title "Ping Pilott agent $VERSION" \
    --notes "Host agent $VERSION. Install it from the server's page in Ping Pilott (Host metrics), or see the README.

Every file is listed with its SHA-256 in \`checksums.txt\`; the installer refuses a file that does not match."
git fetch -q --tags origin
printf '\nPublished: %s\n' "$(gh release view "$VERSION" --json url --jq .url)"
