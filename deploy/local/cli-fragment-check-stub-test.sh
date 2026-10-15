#!/usr/bin/env bash
# cli-fragment-check.sh against fake npm packages built from fixtures.
#
#   deploy/local/cli-fragment-check-stub-test.sh
#
# What is pinned here is that "nothing missing" can be believed:
#   - a fragment present in the pinned package and absent from the latest is reported
#     `missing`, and the same fixture with the fragment kept passes (case 2 is the
#     positive control of case 1: without it "reported" could mean "always reported");
#   - a fragment the bundle only assembles at run time is absent from the pinned package
#     too, so it is `unverifiable` and never a false `missing`, and never a false `ok`;
#   - a search that found nothing even in the pinned package is an error, not a pass;
#   - a download failure, a malformed registry row and an unknown recipe each get their
#     own exit code instead of an empty result.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHECK="$HERE/cli-fragment-check.sh"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
STUB="$WORK/bin"; FIX="$WORK/pkgs"
mkdir -p "$STUB" "$FIX"

fail() {
  echo "NG: $1"
  for f in out err; do [ -s "$WORK/$f" ] && { echo "--- $f ---"; cat "$WORK/$f"; }; done
  exit 1
}

# `npm pack <pkg>@<version> --silent --pack-destination <dir>` over a directory of
# fixtures: $STUB_PKGS/<pkg with / and @ as _>@<version>/ is the package's content.
cat > "$STUB/npm" <<'FAKE'
#!/usr/bin/env bash
[ "$1" = pack ] || { echo "unexpected npm call: $*" >&2; exit 1; }
spec="$2"; dest=""; prev=""
for a in "$@"; do [ "$prev" = --pack-destination ] && dest="$a"; prev="$a"; done
[ -z "${STUB_NPM_FAIL:-}" ] || { echo "npm error 404 $spec" >&2; exit 1; }
dir="$STUB_PKGS/$(printf '%s' "${spec%@*}" | tr '/@' '__')@${spec##*@}"
[ -d "$dir" ] || { echo "npm error 404 $spec" >&2; exit 1; }
name="$(basename "$dir").tgz"
tar -czf "$dest/$name" -C "$dir" --transform 's|^\.|package|' .
echo "$name"
FAKE
chmod +x "$STUB/npm"
export NPM="$STUB/npm" STUB_PKGS="$FIX"

tab=$'\t'
REG="$WORK/registry.txt"
cat > "$REG" <<R
# a comment, then a blank line

claude${tab}run /login${tab}errors.go
claude${tab}Esc to cancel${tab}tmuxx.go
claude${tab}Re-authenticate${tab}errors.go: split in the bundle below
codex${tab}Not logged in${tab}auth.go: this kind has no unpack recipe
R
export FRAGMENTS="$REG"

# pkg <version> <main file content> <string>...: both packages of the recipe. The platform
# package is a compiled binary: each string sits between NULs and non-UTF-8 bytes.
pkg() {
  local v="$1" main="$2" d s
  shift 2
  d="$FIX/_anthropic-ai_claude-code@$v"; rm -rf "$d"; mkdir -p "$d"
  printf '%s' "$main" > "$d/cli.js"
  d="$FIX/_anthropic-ai_claude-code-linux-x64@$v"; rm -rf "$d"; mkdir -p "$d"
  : > "$d/claude"
  for s in "$@"; do printf '\x00\xff\x01%s\x00\x02' "$s" >> "$d/claude"; done
}

rc=0
run() { # run <pinned> <latest>
  set +e
  "$CHECK" check claude "$1" "$2" > "$WORK/out" 2> "$WORK/err"
  rc=$?
  set -e
}
code_is() { [ "$rc" = "$1" ] || fail "expected exit $1, got $rc"; }
line_is() { grep -qxF -- "$1" "$WORK/out" || fail "output lacks the line: $1"; }
line_isnt() { if grep -qxF -- "$1" "$WORK/out"; then fail "output must not have the line: $1"; fi; }

# Re-authenticate is split in both versions; the others are whole in the pinned one.
SPLIT='"Re-" + "authenticate"'

echo "== case 1: a fragment gone from the latest package is reported by name =="
pkg 1.0.0 "x" 'a run /login b' 'Esc to cancel' "$SPLIT"
pkg 1.0.1 "x" 'a run /login b' 'something reworded' "$SPLIT"
run 1.0.0 1.0.1
code_is 1
line_is "missing${tab}Esc to cancel${tab}tmuxx.go"
line_is "ok${tab}run /login${tab}errors.go"

echo "== case 2: CONTROL -- the same fixtures with the fragment kept pass =="
pkg 1.0.1 "x" 'a run /login b' 'Esc to cancel (new layout)' "$SPLIT"
run 1.0.0 1.0.1
code_is 0
line_is "ok${tab}Esc to cancel${tab}tmuxx.go"
grep -q '^missing' "$WORK/out" && fail "nothing is missing here, yet something was reported"

echo "== case 3: a fragment the bundle assembles at run time is unverifiable, not missing and not ok =="
line_is "unverifiable${tab}Re-authenticate${tab}errors.go: split in the bundle below"
line_isnt "ok${tab}Re-authenticate${tab}errors.go: split in the bundle below"

echo "== case 4: a fragment in the MAIN package (older layout) is found as well =="
pkg 1.0.0 "main has Esc to cancel" 'run /login'
pkg 1.0.1 "main has Esc to cancel" 'run /login'
run 1.0.0 1.0.1
code_is 0
line_is "ok${tab}Esc to cancel${tab}tmuxx.go"

echo "== case 5: a search that finds nothing even in the pinned package is an error, not a pass =="
pkg 1.0.0 "x" 'nothing registered is here'
pkg 1.0.1 "x" 'nor here'
run 1.0.0 1.0.1
code_is 2
grep -q 'cannot vouch' "$WORK/err" || fail "the error does not say why"

echo "== case 6: a package that cannot be fetched is an error naming it =="
STUB_NPM_FAIL=1 run 1.0.0 1.0.1
code_is 2
grep -q 'could not fetch @anthropic-ai/claude-code@1.0.0' "$WORK/err" || fail "the error does not name the package"
pkg 1.0.0 "x" 'run /login'
rm -rf "$FIX/_anthropic-ai_claude-code-linux-x64@1.0.1"
run 1.0.0 1.0.1
code_is 2

echo "== case 7: a kind with registry rows but no recipe is 'not checked' (3), never a pass =="
set +e; "$CHECK" check codex 0.1.0 0.2.0 > "$WORK/out" 2> "$WORK/err"; rc=$?; set -e
code_is 3
grep -q 'no unpack recipe' "$WORK/err" || fail "the message does not say why"

echo "== case 8: a malformed registry row stops the run =="
printf 'claude%srun /login\n' "$tab" > "$WORK/bad.txt"     # no "why" column
set +e; FRAGMENTS="$WORK/bad.txt" "$CHECK" check claude 1.0.0 1.0.1 > "$WORK/out" 2> "$WORK/err"; rc=$?; set -e
code_is 2
grep -q 'malformed registry row' "$WORK/err" || fail "the message does not say why"
set +e; FRAGMENTS="$WORK/bad.txt" "$CHECK" has claude > "$WORK/out" 2> "$WORK/err"; rc=$?; set -e
code_is 2   # `has` must not read a broken registry as "no rows"

echo "== case 9: has =="
"$CHECK" has claude || fail "has claude"
if "$CHECK" has kiro; then fail "has kiro must be false"; fi

echo "== case 10: the shipped registry is well-formed and every fragment is whole ASCII text =="
FRAGMENTS="$HERE/cli-message-fragments.txt" "$CHECK" has claude || fail "the shipped registry has no claude rows"
bad="$(grep -v '^#' "$HERE/cli-message-fragments.txt" | grep -v '^$' | awk -F'\t' 'NF != 3 || $2 !~ /^[ -~]+$/ { print }')"
[ -z "$bad" ] || fail "malformed or non-ASCII registry rows: $bad"

echo "OK: cli-fragment-check.sh reports a gone fragment, never mistakes an unsearchable one for either answer"
