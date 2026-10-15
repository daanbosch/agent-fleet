#!/usr/bin/env bash
# Do the upstream message fragments Agent Fleet matches on still exist in a CLI release?
#
#   cli-fragment-check.sh has <kind>                      exit 0 when the registry has rows for it
#   cli-fragment-check.sh check <kind> <pinned> <latest>  unpack both packages and look
#
# cli-pin-bump.sh calls `check` for every CLI whose pin is behind. The contract tests run
# the real CLI but never read its wording, so a reworded auth error or TUI footer leaves CI
# green while detection stops working (workspace/agent/internal/tmuxx/testdata/footers/
# SOURCE.txt: three silent breaks). This is a "go look here" signal like the drift check
# itself; it decides nothing.
#
# ## Registry
#
# cli-message-fragments.txt: `<kind> TAB <fragment> TAB <why>` per row, one registry for
# every CLI. Rows for a kind with no unpack recipe (RECIPE below) exit 3, "not checked".
#
# ## What is compared
#
# Each registered fragment is searched with `grep -aF` (bytes, no regex) over every file of
# the unpacked package(s), for the PINNED and the LATEST version. Each fragment gets one of:
#
#   ok            found in latest (and in pinned)
#   missing       found in pinned, absent from latest: the signal. A reworded or removed
#                 message.
#   unverifiable  absent from pinned already. The search cannot vouch for it either way,
#                 so a verdict on latest would be noise: a fragment the bundle spells in
#                 pieces ("a" + "b"), writes as unicode escapes or stores as UTF-16
#                 never matches, and the pinned version is the control that says so. It is
#                 listed so the row gets fixed; it is never reported as missing.
#
# This is the false-negative policy: a plain byte search cannot see a fragment that is
# assembled at run time, so such a fragment is detected on the pinned side and demoted
# instead of silently passing. The converse hole stays: a fragment that is present in
# latest only as part of a different message still counts as found.
#
# Exit codes (stdout: `status TAB fragment TAB why`, one line per row):
#   0  nothing missing
#   1  at least one `missing`
#   2  the check itself failed (download, unpack, bad registry, or NO fragment could be
#      found in the pinned version: then the search, not the CLI, is what is broken)
#   3  registry rows exist but this kind has no unpack recipe
#
# ## Distributions
#
# claude ships its code as a compiled binary in the platform package
# (@anthropic-ai/claude-code-linux-x64) with string literals stored as plain UTF-8; the
# main package is unpacked too, so an older layout that kept a JS bundle there still
# works. The binary-only CLIs (agy, cursor, kiro, muse) have no npm package and no recipe
# yet; give them one in `packages_of` when a row for them is added.
#
# ## Environment
#
#   FRAGMENTS  the registry (default: cli-message-fragments.txt beside this script)
#   NPM        the npm to run (default: npm); `pack` is the only subcommand used
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FRAGMENTS="${FRAGMENTS:-$HERE/cli-message-fragments.txt}"
NPM="${NPM:-npm}"

die() { echo "cli-fragment-check: $*" >&2; exit 2; }

# The npm packages whose contents carry the CLI's strings, one per line; empty = no recipe.
packages_of() {
  case "$1" in
    claude) printf '%s\n' @anthropic-ai/claude-code @anthropic-ai/claude-code-linux-x64 ;;
  esac
}

# Registry rows of one kind as `fragment TAB why`; fails on a malformed row, so a typo
# cannot silently drop a fragment from the check.
rows_of() {
  [ -f "$FRAGMENTS" ] || die "no registry at $FRAGMENTS"
  local line k f w
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in ''|'#'*) continue ;; esac
    IFS=$'\t' read -r k f w <<< "$line"
    [ -n "$k" ] && [ -n "$f" ] && [ -n "$w" ] || die "malformed registry row (want kind TAB fragment TAB why): $line"
    [[ "$k" =~ ^[a-z0-9-]+$ ]] || die "invalid kind in registry row: $line"
    case "$f" in *'`'*) die "a fragment must not contain a backtick: $line" ;; esac
    [ "$k" != "$1" ] || printf '%s\t%s\n' "$f" "$w"
  done < "$FRAGMENTS"
}

cmd="${1:-}"
case "$cmd" in
  has)
    kind="${2:?kind}"
    # A malformed registry must stop the run, not read as "no rows for this kind".
    r="$(rows_of "$kind")" || exit 2
    [ -n "$r" ]
    exit
    ;;
  check) ;;
  *) echo "usage: $0 has <kind> | check <kind> <pinned> <latest>" >&2; exit 2 ;;
esac

kind="${2:?kind}"; pinned="${3:?pinned}"; latest="${4:?latest}"
[[ "$kind" =~ ^[a-z0-9-]+$ ]] || die "invalid kind: $kind"
for v in "$pinned" "$latest"; do
  [[ "$v" =~ ^[0-9A-Za-z._+-]+$ ]] || die "invalid version: $v"
done

rows="$(rows_of "$kind")" || exit 2
[ -n "$rows" ] || die "no registry rows for $kind"
pkgs="$(packages_of "$kind")"
if [ -z "$pkgs" ]; then
  echo "no unpack recipe for $kind (binary-only or not yet taught); its fragments were not checked" >&2
  exit 3
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# unpack <version> <dir>: every package of the recipe at that version, extracted side by
# side. Downloaded files are data: extracted into a fresh directory, never executed.
unpack() {
  local v="$1" dir="$2" pkg tgz
  mkdir -p "$dir" "$WORK/dl"
  while IFS= read -r pkg; do
    tgz="$("$NPM" pack "$pkg@$v" --silent --pack-destination "$WORK/dl" 2>"$WORK/npm.err" | tail -n 1)" ||
      die "could not fetch $pkg@$v: $(tr '\n' ' ' < "$WORK/npm.err")"
    [ -n "$tgz" ] && [ -f "$WORK/dl/$tgz" ] || die "npm pack produced no file for $pkg@$v"
    mkdir -p "$dir/$(printf '%s' "$pkg" | tr '/@' '__')"
    tar -xzf "$WORK/dl/$tgz" -C "$dir/$(printf '%s' "$pkg" | tr '/@' '__')" || die "could not unpack $pkg@$v"
    rm -f "$WORK/dl/$tgz"
  done <<< "$pkgs"
  [ -n "$(find "$dir" -type f -print -quit)" ] || die "the $v package holds no files"
}

found_in() { grep -raFq -e "$1" "$2"; } # found_in <fragment> <dir>

unpack "$pinned" "$WORK/pinned"
unpack "$latest" "$WORK/latest"

ok=0 missing=0
while IFS=$'\t' read -r frag why; do
  if found_in "$frag" "$WORK/pinned"; then
    if found_in "$frag" "$WORK/latest"; then
      status=ok; ok=$((ok + 1))
    else
      status=missing; missing=$((missing + 1))
    fi
  else
    status=unverifiable
  fi
  printf '%s\t%s\t%s\n' "$status" "$frag" "$why"
done <<< "$rows"

# Not one fragment found in the version the code was written against: the unpack or the
# search is broken, and "nothing missing" would be an empty result, not a pass.
[ "$ok" -gt 0 ] || [ "$missing" -gt 0 ] || die "none of the registered fragments was found in $kind $pinned; the check cannot vouch for anything"
[ "$missing" -eq 0 ] || exit 1
