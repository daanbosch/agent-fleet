#!/usr/bin/env bash
# apt-pin-bump.sh against fixture package indexes served over file://, with the real
# apt-pin-check.sh underneath and a copy of the real Dockerfile.
#
#   deploy/local/apt-pin-bump-test.sh
#
# The bump PR is opened unattended, so what it proposes is pinned here: the newest build
# served on both architectures and nothing else (case 1), never a downgrade (case 4), and
# nothing at all when an index could not be read (case 5). Case 1 is the positive control:
# it must change the file, or every "unchanged" below proves nothing.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
BUMP="$HERE/apt-pin-bump.sh"

PIN="$(sed -n 's/^ARG CHROMIUM_VERSION=//p' "$ROOT/workspace/Dockerfile")"
[ -n "$PIN" ] || { echo "NG: ARG CHROMIUM_VERSION not found in workspace/Dockerfile"; exit 1; }
NEWER="999.0.0.1-1~deb13u1"
OLDER="1.0.0.1-1~deb13u1"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

fail() {
  echo "NG: $CASE: $1"
  for f in out err gh_out body.md; do
    [ -s "$WORK/$f" ] || continue
    echo "--- $f ---"; cat "$WORK/$f"
  done
  exit 1
}

index() { # index <tree> <suite> <arch> [<package> <version>]...
  local tree="$1" suite="$2" arch="$3"; shift 3
  local dir="$WORK/$tree/dists/$suite/main/binary-$arch"
  mkdir -p "$dir"
  : > "$dir/Packages"
  while [ "$#" -gt 0 ]; do
    printf 'Package: %s\nSource: chromium\nVersion: %s\nArchitecture: %s\n\n' "$1" "$2" "$arch" >> "$dir/Packages"
    shift 2
  done
  xz -f "$dir/Packages"
}
all3() { echo chromium "$1" chromium-common "$1" chromium-sandbox "$1"; }

# A case: fresh trees with readable, chromium-free main suites, and a fresh Dockerfile copy.
reset() {
  rm -rf "$WORK/debian" "$WORK/security"
  for a in amd64 arm64; do
    index debian trixie "$a"
    index debian trixie-updates "$a"
  done
  cp "$ROOT/workspace/Dockerfile" "$WORK/Dockerfile"
}

run() {
  : > "$WORK/gh_out"
  set +e
  DOCKERFILE="$WORK/Dockerfile" BODY_FILE="$WORK/body.md" GITHUB_OUTPUT="$WORK/gh_out" \
  APT_PIN_DEBIAN_URL="file://$WORK/debian" APT_PIN_SECURITY_URL="file://$WORK/security" \
    "$BUMP" > "$WORK/out" 2> "$WORK/err"
  code=$?
  set -e
  [ "$code" = 0 ] || fail "exit $code, want 0"
}
output_is() { grep -qxF -- "$1" "$WORK/gh_out" || fail "GITHUB_OUTPUT lacks '$1'"; }
unchanged() { cmp -s "$ROOT/workspace/Dockerfile" "$WORK/Dockerfile" || fail "the Dockerfile changed"; }

# shellcheck disable=SC2046
{
CASE="1 the pin is gone and a newer build is served on both -> exactly that ARG line moves"
reset
index security trixie-security amd64 $(all3 "$NEWER")
index security trixie-security arm64 $(all3 "$NEWER")
run
output_is "count=1"
output_is "incomplete=false"
output_is "title=build(workspace): bump CHROMIUM_VERSION to $NEWER"
grep -q '^edit_id=[0-9a-f]\{16\}$' "$WORK/gh_out" || fail "no edit_id"
diffs="$({ diff "$ROOT/workspace/Dockerfile" "$WORK/Dockerfile" || true; } | grep '^[<>]')"
[ "$diffs" = "< ARG CHROMIUM_VERSION=$PIN
> ARG CHROMIUM_VERSION=$NEWER" ] || fail "unexpected diff: $diffs"
grep -qF "| \`CHROMIUM_VERSION\` | chromium chromium-common chromium-sandbox | \`$PIN\` → \`$NEWER\` |" "$WORK/body.md" || fail "no evidence row"
grep -qF "<!-- apt-pin-bump edit=" "$WORK/body.md" || fail "no edit marker"

CASE="2 a second run against the bumped file changes nothing"
cp "$WORK/Dockerfile" "$WORK/bumped"
DOCKERFILE_BEFORE="$WORK/bumped"
run
output_is "count=0"
cmp -s "$DOCKERFILE_BEFORE" "$WORK/Dockerfile" || fail "the second run changed the file"

CASE="3 the newer build reached one architecture only -> nothing moves yet"
reset
index security trixie-security amd64 $(all3 "$NEWER")
index security trixie-security arm64 $(all3 "$PIN")
run
output_is "count=0"
unchanged

CASE="4 the newest build served on both is older than the pin -> never a downgrade"
reset
index security trixie-security amd64 $(all3 "$PIN") $(all3 "$OLDER")
index security trixie-security arm64 $(all3 "$OLDER")
run
output_is "count=0"
grep -qF "not downgrading" "$WORK/out" || fail "the downgrade was not reported"
unchanged

CASE="5 one index unreadable -> incomplete, nothing moves even though a newer build is listed"
reset
rm -rf "$WORK/debian/dists/trixie-updates/main/binary-arm64"
index security trixie-security amd64 $(all3 "$NEWER")
index security trixie-security arm64 $(all3 "$NEWER")
run
output_is "incomplete=true"
output_is "count=0"
unchanged
}

echo "OK: apt-pin-bump.sh (5 cases)"
