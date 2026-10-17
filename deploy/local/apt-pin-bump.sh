#!/usr/bin/env bash
# Rewrite the Debian package pins in workspace/Dockerfile to the newest build served on
# every architecture, and write the body of the bump PR.
#
#   deploy/local/apt-pin-bump.sh
#
# apt-pin-bump.yml calls this and does nothing else to the tree; the decision lives here so
# that apt-pin-bump-test.sh can walk it against fixture indexes.
#
# ## Why a bump is always the answer
#
# trixie-security keeps only the current build, so a pin that stops resolving means Debian
# shipped a security update, and the update is what the image should carry. The newest
# version is taken from apt-pin-check.sh, which already reads every suite on every
# architecture and names the newest version served for all of a pin's packages on all of
# them: that is the only value a build of either architecture can install.
#
# ## Which pins move
#
# A pin written as `pkg=${ARG}` moves when that newest version is NEWER than the pin (dpkg
# ordering). Equal is nothing to do. Older happens when the pin is the fresh security build
# on one architecture only while the other still serves an older one from the main archive;
# moving there would be a downgrade, so the pin is left for the day both architectures
# serve it. A literal pin (`pkg=1.2-3`) is never rewritten: it has no ARG line to edit.
#
# An index that could not be read makes the run incomplete (`incomplete=true`): the newest
# version it names may be stale, so the workflow leaves the branch and the PR as they are.
#
# ## Guarantees
#
# - Only the `ARG <name>=` lines of the moved pins change; the rewrite is checked line by
#   line before it replaces the file, in one rename.
# - A second run changes nothing.
#
# ## Environment
#
#   DOCKERFILE     file to rewrite (default: workspace/Dockerfile)
#   APT_PIN_CHECK  the verdict source (default: apt-pin-check.sh; APT_PIN_* pass through)
#   BODY_FILE      where the PR body is written (default: none)
#   GITHUB_OUTPUT  count=, title=, edit_id=, incomplete=
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
DOCKERFILE="${DOCKERFILE:-$ROOT/workspace/Dockerfile}"
APT_PIN_CHECK="${APT_PIN_CHECK:-$HERE/apt-pin-check.sh}"
BODY_FILE="${BODY_FILE:-}"
OUT="${GITHUB_OUTPUT:-/dev/null}"
VERSION_RE='^[0-9][0-9A-Za-z.+~:-]*$'

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

die() { echo "apt-pin-bump: $*" >&2; exit 1; }

[ -f "$DOCKERFILE" ] || die "no Dockerfile at $DOCKERFILE"

set +e
APT_PIN_DOCKERFILE="$DOCKERFILE" "$APT_PIN_CHECK" > "$WORK/check.txt" 2> "$WORK/check.err"
rc=$?
set -e
case "$rc" in 0|1|2) ;; *) cat "$WORK/check.txt" "$WORK/check.err" >&2; die "apt-pin-check.sh exited $rc" ;; esac

incomplete=false
grep -q '^Incomplete:' "$WORK/check.txt" && incomplete=true

: > "$WORK/edits"
rows=()
# `<ARG>: pinned <pin>; newest version served for all of [<pkgs>] on [<arches>]: <newest>`
while IFS='|' read -r arg pin pkgs arches newest; do
  [ -n "$arg" ] || continue
  [ "$newest" != none ] || { echo "$arg: no version is served for all of [$pkgs] on [$arches]; left at $pin"; continue; }
  [[ "$newest" =~ $VERSION_RE ]] || die "refusing an unexpected version for $arg: $newest"
  [ "$newest" != "$pin" ] || continue
  if ! dpkg --compare-versions "$newest" gt "$pin"; then
    echo "$arg: $newest is older than the pin $pin; not downgrading"
    continue
  fi
  n="$(grep -c "^ARG $arg=" "$DOCKERFILE" || true)"
  [ "$n" = 1 ] || die "ARG $arg must appear exactly once in $DOCKERFILE (found $n)"
  printf '%s=%s\n' "$arg" "$newest" >> "$WORK/edits"
  rows+=("| \`$arg\` | $pkgs | \`$pin\` → \`$newest\` | $arches |")
done < <(sed -n 's/^\([A-Z_][A-Z0-9_]*\): pinned \([^;]*\); newest version served for all of \[\([^]]*\)\] on \[\([^]]*\)\]: \(.*\)$/\1|\2|\3|\4|\5/p' "$WORK/check.txt")

count="$(wc -l < "$WORK/edits")"
if [ "$incomplete" = true ]; then count=0; fi

if [ "$count" -gt 0 ]; then
  tmp="$(mktemp "$(dirname "$DOCKERFILE")/.Dockerfile.aptbump.XXXXXX")"
  trap 'rm -rf "$WORK"; rm -f "$tmp"' EXIT
  awk -v edits="$WORK/edits" '
    BEGIN { while ((getline l < edits) > 0) { i = index(l, "="); v[substr(l, 1, i - 1)] = substr(l, i + 1) } }
    /^ARG [A-Za-z0-9_]+=/ {
      i = index($0, "="); k = substr($0, 5, i - 5)
      if (k in v) { print "ARG " k "=" v[k]; next }
    }
    { print }
  ' "$DOCKERFILE" > "$tmp"
  [ "$(wc -l < "$tmp")" = "$(wc -l < "$DOCKERFILE")" ] || die "the rewrite changed the line count"
  diffs="$({ diff "$DOCKERFILE" "$tmp" || true; } | sed -n 's/^> //p' | sort)"
  want="$(sed 's/^/ARG /' "$WORK/edits" | sort)"
  [ "$diffs" = "$want" ] || die "the rewrite does not match the planned edit"
  chmod --reference="$DOCKERFILE" "$tmp"
  mv "$tmp" "$DOCKERFILE"
fi

title=""
edit_id=""
if [ "$count" -gt 0 ]; then
  title="build(workspace): bump $(cut -d= -f1 "$WORK/edits" | paste -sd, - | sed 's/,/, /g') to $(cut -d= -f2 "$WORK/edits" | paste -sd, - | sed 's/,/, /g')"
  edit_id="$(sort "$WORK/edits" | sha256sum | cut -c1-16)"
fi
{
  printf 'count=%s\n' "$count"
  printf 'incomplete=%s\n' "$incomplete"
  printf 'title=%s\n' "$title"
  printf 'edit_id=%s\n' "$edit_id"
} >> "$OUT"

if [ -n "$BODY_FILE" ]; then
  {
    echo "Bumps the Debian package pins in \`workspace/Dockerfile\` to the newest build served on"
    echo "every architecture. trixie-security keeps only the current build, so the old pin no"
    echo "longer installs (or soon will not) and this is the security update."
    echo
    echo "| ARG | Packages | Pin | Served on |"
    echo "|---|---|---|---|"
    printf '%s\n' "${rows[@]-}"
    echo
    echo '<details><summary><code>apt-pin-check.sh</code> on the old pin</summary>'
    echo
    echo '```'
    cat "$WORK/check.txt"
    echo '```'
    echo '</details>'
    echo
    echo "Evidence is the workspace image build \`apt-pin-bump.yml\` starts on this branch for"
    echo "amd64 and arm64 (linked below): the Dockerfile asserts the installed package version,"
    echo "\`chromium --version\` and the sandbox mode. Merge once it is green."
    echo
    echo "Opened by \`apt-pin-bump.yml\` (\`deploy/local/apt-pin-bump.sh\`). Nothing merges it"
    echo "automatically. Closing it without merging declines this exact edit until a newer build"
    echo "is published."
    echo
    echo "<!-- apt-pin-bump edit=$edit_id -->"
  } > "$BODY_FILE"
fi

if [ "$count" -gt 0 ]; then
  sed 's/^/bumped: /' "$WORK/edits"
elif [ "$incomplete" = true ]; then
  echo "incomplete: an index could not be read; nothing changed"
else
  echo "nothing to bump"
fi
