#!/usr/bin/env bash
# Positive and negative controls for scripts/migration-versions.sh.
#
# The stdin mode runs against this repository's own history: the base just before #2039
# merged already had 0095_member_ui_state.sql, so #2039's 0095_handoff_take.sql must be
# flagged there, and a file the base already has, or a free version, must not be.
#
# The PR modes run in a throwaway origin/clone pair with a stub gh that answers from JSON
# fixtures through the script's own --jq filters and records every status it is asked to set.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/migration-versions.sh"
FAILED=0
fail() {
  echo "FAIL: $*"
  FAILED=1
}

base=dc3ebb497 # develop after #2043 (member_ui_state), before #2039 (handoff_take)
git cat-file -e "$base^{commit}" 2>/dev/null || {
  echo "FAIL: $base is not in this clone (fetch-depth too shallow?)"
  exit 1
}

check() {
  local want="$1" input="$2" rc=0 ref="${3:-$base}"
  printf '%b' "$input" | "$SCRIPT" "$ref" > /dev/null 2>&1 || rc=$?
  [ "$rc" = "$want" ] || fail "want exit $want, got $rc for: $(printf '%b' "$input" | tr '\n' ' ')"
}

M=control-plane/internal/store/migrations
P=control-plane/internal/store/migrations-pg
check 1 "$M/0095_handoff_take.sql\n"
check 1 "$P/0080_handoff_take.sql\n"
check 1 "docs/a.md\n$P/0080_handoff_take.sql\n"
check 0 "$M/0095_member_ui_state.sql\n"
check 0 "$M/0096_handoff_take.sql\n"
check 0 "$P/0081_handoff_take.sql\n"
check 0 "control-plane/session_handoff.go\n"
check 0 ""
# Two new files of one change taking the same free version.
check 1 "$M/0096_a.sql\n$M/0096_b.sql\n"
check 1 "$P/0081_a.sql\ndocs/a.md\n$P/0081_b.sql\n"
check 0 "$M/0096_a.sql\n$P/0096_a.sql\n"
check 0 "$M/0096_a.sql\n$M/0097_b.sql\n"
check 2 "$M/0096_a.sql\n" no-such-ref

# --- PR modes -----------------------------------------------------------------------------

T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT
g() { git -c core.hooksPath=/dev/null -c commit.gpgsign=false -c user.name=t -c user.email=t@example.invalid "$@"; }
g init -q --bare -b develop "$T/origin.git"
g clone -q "$T/origin.git" "$T/work" 2>/dev/null
mkdir -p "$T/work/$M" "$T/work/$P"
touch "$T/work/$M/0001_a.sql" "$T/work/$P/0001_a.sql"
g -C "$T/work" add -A
g -C "$T/work" commit -qm base
g -C "$T/work" push -q origin HEAD:develop

mkdir "$T/bin" "$T/fx"
cat > "$T/bin/gh" << 'EOF'
#!/usr/bin/env bash
# Stub gh: answers from $FX, applies --jq with jq, records statuses in $FX/statuses.
set -euo pipefail
jqf=. args=() fields=()
while [ $# -gt 0 ]; do
  case "$1" in
    --jq) jqf="$2"; shift 2 ;;
    -f) fields+=("$2"); shift 2 ;;
    *) args+=("$1"); shift ;;
  esac
done
case "${args[*]}" in
  "pr list "*) jq -r "$jqf" "$FX/prs.json" ;;
  "api --paginate repos/o/r/pulls/"*"/files")
    n="${args[2]#repos/o/r/pulls/}" n="${n%/files}"
    [ -f "$FX/pulls-$n.json" ] || { echo "stub gh: HTTP 502" >&2; exit 1; }
    jq -r "$jqf" "$FX/pulls-$n.json" ;;
  "api -X POST repos/o/r/statuses/"*)
    sha="${args[3]#repos/o/r/statuses/}" state= context= desc=
    for f in "${fields[@]}"; do
      case "$f" in
        state=*) state="${f#state=}" ;; context=*) context="${f#context=}" ;; description=*) desc="${f#description=}" ;;
      esac
    done
    [ "${#desc}" -le 140 ] || { echo "stub gh: description over 140" >&2; exit 1; }
    printf '%s %s %s %s\n' "$sha" "$state" "$context" "$desc" >> "$FX/statuses"
    if [ -f "$FX/on-post" ]; then mv "$FX/on-post" "$FX/on-post.ran"; bash "$FX/on-post.ran"; fi
    echo '{}' ;;
  *) echo "stub gh: unexpected: $*" >&2; exit 99 ;;
esac
EOF
chmod +x "$T/bin/gh"

files() { # files <n> <status>:<path>...
  local n="$1" s
  shift
  for s in "$@"; do jq -n --arg st "${s%%:*}" --arg fn "${s#*:}" '{status: $st, filename: $fn}'; done |
    jq -s . > "$T/fx/pulls-$n.json"
}
files 1 added:docs/x.md modified:"$M/0001_a.sql"
files 2 added:"$M/0002_b.sql" added:"$P/0002_b.sql"
files 3 added:"$M/0001_c.sql"
files 4 added:"$M/0002_x.sql" added:"$M/0002_y.sql"
files 5 renamed:"$P/0001_z.sql"
files 6 removed:"$M/0001_a.sql" copied:"$M/0003_q.sql"
# PR 7 has no fixture: listing its files fails.

run() { # run <want-exit> <args...>; output in $T/out
  local want="$1" rc=0
  shift
  : > "$T/fx/statuses"
  (cd "$T/work" && PATH="$T/bin:$PATH" FX="$T/fx" GITHUB_REPOSITORY=o/r "$SCRIPT" "$@") > "$T/out" 2>&1 || rc=$?
  [ "$rc" = "$want" ] || fail "want exit $want, got $rc for $*: $(cat "$T/out")"
}
statuses() { # statuses <want>: the recorded "sha state" lines, in order
  local got
  got="$(cut -d' ' -f1-3 "$T/fx/statuses")"
  [ "$got" = "$(printf '%b' "$1")" ] || fail "statuses: want [$(printf '%b' "$1" | tr '\n' ',')] got [$(tr '\n' ',' <<< "$got")]"
}
has() { grep -qF -- "$2" "$1" || fail "$1 lacks: $2 ($(cat "$1"))"; }

run 0 --pr 1 s1
statuses 's1 success migration-version'
run 0 --pr 2 s2
statuses 's2 success migration-version'
run 1 --pr 3 s3
statuses 's3 failure migration-version'
has "$T/fx/statuses" "migrations/0001_c.sql collides with 0001_a.sql on develop; renumber it"
has "$T/out" "#3: $M/0001_c.sql: version 0001 is already $M/0001_a.sql on "
run 1 --pr 4 s4
statuses 's4 failure migration-version'
has "$T/fx/statuses" "migrations/0002_x.sql collides with 0002_y.sql in this PR (+1 more); renumber it"
run 1 --pr 5 s5
statuses 's5 failure migration-version'
run 0 --pr 6 s6
statuses 's6 success migration-version'
# Without a sha (a fork's read-only token) the verdict is the exit code alone.
run 1 --pr 3
statuses ''
run 0 --pr 2
statuses ''
# Listing the files failed: no status at all, never a success.
run 2 --pr 7 s7
statuses ''

# develop takes 0002 while PR 2's check runs: the first verdict is stale, the re-run flags it.
cat > "$T/fx/on-post" << EOF
set -e
cd "$T" && rm -rf mover && git clone -q origin.git mover 2>/dev/null && cd mover
touch $M/0002_other.sql
git -c core.hooksPath=/dev/null -c commit.gpgsign=false -c user.name=t -c user.email=t@example.invalid add -A
git -c core.hooksPath=/dev/null -c commit.gpgsign=false -c user.name=t -c user.email=t@example.invalid commit -qm move
git push -q origin HEAD:develop
EOF
run 1 --pr 2 s2
statuses 's2 success migration-version\ns2 failure migration-version'
has "$T/out" "develop moved past"
[ ! -f "$T/fx/on-post" ] || fail "the develop-moves hook never ran"
g -C "$T/work" fetch -q origin develop

# --open-prs: every open PR gets a status, migration or not; collisions do not fail the run.
jq -n '[range(1;7) | {number: ., headRefOid: "h\(.)"}]' > "$T/fx/prs.json"
run 0 --open-prs origin/develop
statuses 'h1 success migration-version\nh2 failure migration-version\nh3 failure migration-version\nh4 failure migration-version\nh5 failure migration-version\nh6 success migration-version'
# One PR that cannot be checked fails the run, and the others still get theirs.
jq -n '[7, 1] | map({number: ., headRefOid: "h\(.)"})' > "$T/fx/prs.json"
run 1 --open-prs origin/develop
statuses 'h1 success migration-version'
echo '[]' > "$T/fx/prs.json"
run 0 --open-prs origin/develop
statuses ''

exit "$FAILED"
