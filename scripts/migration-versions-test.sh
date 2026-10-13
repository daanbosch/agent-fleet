#!/usr/bin/env bash
# Positive and negative controls for scripts/migration-versions.sh.
#
# The stdin mode runs against this repository's own history: the base just before #2039
# merged already had 0095_member_ui_state.sql, so #2039's 0095_handoff_take.sql must be
# flagged there, and a file the base already has, or a free version, must not be.
#
# The PR modes run in a throwaway origin/clone pair whose PR heads are real commits under
# refs/pull/<n>/head, with a stub gh that answers `pr list` from a JSON fixture through the
# script's own --jq filter and records every status it is asked to set. A hook the stub runs
# on each post moves develop under the script, to race it the way a concurrent run does.
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
# Versions compare as the runner's strconv.Atoi reads them, and an unreadable one fails.
check 1 "$M/0096_a.sql\n$M/96_b.sql\n"
check 1 "$M/95_x.sql\n"
check 1 "$M/+0095_x.sql\n"
check 1 "$M/0096_a.sql\n$M/0096_a.down.sql\n"
check 1 "$M/0096.sql\n"
check 1 "$M/abc_x.sql\n"
check 0 "$M/96_b.sql\n"
check 0 "$M/sub/0095_x.sql\n"
check 0 "$M/0095_x.txt\n"
check 2 "$M/0096_a.sql\n" no-such-ref

# --- PR modes -----------------------------------------------------------------------------

T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT
g() { git -c core.hooksPath=/dev/null -c commit.gpgsign=false -c user.name=t -c user.email=t@example.invalid "$@"; }
g init -q --bare -b develop "$T/origin.git"
g clone -q "$T/origin.git" "$T/work" 2>/dev/null
mkdir -p "$T/work/$M" "$T/work/$P" "$T/work/docs"
touch "$T/work/$M/0001_a.sql" "$T/work/$P/0001_a.sql" "$T/work/docs/seed.sql"
g -C "$T/work" add -A
g -C "$T/work" commit -qm base
g -C "$T/work" push -q origin HEAD:develop
BASE="$(git -C "$T/work" rev-parse HEAD)"

# mkpr <n> <from> <shell commands>: a commit on <from> made by the commands, pushed as
# refs/pull/<n>/head; prints its sha. The commands run in the work tree.
mkpr() {
  g -C "$T/work" checkout -q --detach "$2"
  (set -e && cd "$T/work" && eval "$3")
  g -C "$T/work" add -A
  g -C "$T/work" commit -qm "pr $1"
  g -C "$T/work" push -q -f origin "HEAD:refs/pull/$1/head"
  git -C "$T/work" rev-parse HEAD
}
S1="$(mkpr 1 "$BASE" "echo x > docs/x.md; echo y > $M/0001_a.sql")"
S2="$(mkpr 2 "$BASE" "touch $M/0002_b.sql $P/0002_b.sql")"
S3="$(mkpr 3 "$BASE" "touch $M/0001_c.sql")"
S4="$(mkpr 4 "$BASE" "touch $M/0002_x.sql $M/0002_y.sql")"
S5="$(mkpr 5 "$BASE" "git mv docs/seed.sql $P/0001_z.sql")"
S6="$(mkpr 6 "$BASE" "touch $M/0003_q.sql; git rm -q $M/0001_a.sql")"
S9="$(mkpr 9 "$BASE" "touch $M/1_x.sql")"
# PR 8's event was for A (colliding); the PR has since moved on to B, which drops the file.
A8="$(mkpr 8 "$BASE" "touch $M/0001_c.sql")"
B8="$(mkpr 8 "$A8" "git rm -q $M/0001_c.sql; touch docs/y.md")"
S7=0123456789abcdef0123456789abcdef01234567 # in no repository
# The clone the script runs in has none of the PR heads: it must fetch them.
rm -rf "$T/work"
g clone -q "$T/origin.git" "$T/work" 2>/dev/null

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
  "api -X POST repos/o/r/statuses/"*)
    [ ! -f "$FX/post-fails" ] || { echo "stub gh: HTTP 502" >&2; exit 1; }
    sha="${args[3]#repos/o/r/statuses/}" state= context= desc=
    for f in "${fields[@]}"; do
      case "$f" in
        state=*) state="${f#state=}" ;; context=*) context="${f#context=}" ;; description=*) desc="${f#description=}" ;;
      esac
    done
    [ "${#desc}" -le 140 ] || { echo "stub gh: description over 140" >&2; exit 1; }
    printf '%s %s %s %s\n' "$sha" "$state" "$context" "$desc" >> "$FX/statuses"
    if [ -f "$FX/on-post" ]; then bash "$FX/on-post"; fi
    echo '{}' ;;
  *) echo "stub gh: unexpected: $*" >&2; exit 99 ;;
esac
EOF
chmod +x "$T/bin/gh"

run() { # run <want-exit> <args...>; develop reset to BASE first, output in $T/out
  local want="$1" rc=0
  shift
  : > "$T/fx/statuses"
  git -C "$T/origin.git" update-ref refs/heads/develop "$BASE"
  (cd "$T/work" && PATH="$T/bin:$PATH" FX="$T/fx" GITHUB_REPOSITORY=o/r "$SCRIPT" "$@") > "$T/out" 2>&1 || rc=$?
  [ "$rc" = "$want" ] || fail "want exit $want, got $rc for $*: $(cat "$T/out")"
  rm -f "$T/fx/on-post" "$T/fx/post-fails"
}
statuses() { # statuses <want>: the recorded "sha state" lines, in order
  local got
  got="$(cut -d' ' -f1-2 "$T/fx/statuses")"
  [ "$got" = "$(printf '%b' "$1")" ] || fail "statuses: want [$(printf '%b' "$1" | tr '\n' ',')] got [$(tr '\n' ',' <<< "$got")]"
  ! grep -qv '^[^ ]* [^ ]* migration-version ' "$T/fx/statuses" || fail "a status with another context: $(cat "$T/fx/statuses")"
}
has() { grep -qF -- "$2" "$1" || fail "$1 lacks: $2 ($(cat "$1"))"; }
# mover [keep]: an on-post hook that pushes one more commit to develop; one-shot unless keep.
# Extra lines on stdin are run in the mover's tree before it commits.
mover() {
  {
    echo 'set -e'
    [ "${1:-}" = keep ] || echo "rm -f '$T/fx/on-post'"
    echo "cd '$T' && rm -rf mover && git clone -q origin.git mover 2>/dev/null && cd mover"
    cat
    # shellcheck disable=SC2016 # expands in the hook
    echo 'echo "$RANDOM" >> docs/moves'
    echo "git -c core.hooksPath=/dev/null -c commit.gpgsign=false -c user.name=t -c user.email=t@example.invalid add -A"
    echo "git -c core.hooksPath=/dev/null -c commit.gpgsign=false -c user.name=t -c user.email=t@example.invalid commit -qm move"
    echo 'git push -q origin HEAD:develop'
  } > "$T/fx/on-post"
}

run 0 --pr 1 "$S1"
statuses "$S1 success"
run 0 --pr 2 "$S2"
statuses "$S2 success"
run 1 --pr 3 "$S3"
statuses "$S3 failure"
has "$T/fx/statuses" "migrations/0001_c.sql collides with 0001_a.sql on develop; renumber it"
has "$T/out" "#3: $M/0001_c.sql: version 1 is already $M/0001_a.sql on develop"
run 1 --pr 4 "$S4"
statuses "$S4 failure"
has "$T/fx/statuses" "migrations/0002_x.sql collides with 0002_y.sql in this PR (+1 more); renumber it"
run 1 --pr 5 "$S5"
statuses "$S5 failure"
run 0 --pr 6 "$S6"
statuses "$S6 success"
run 1 --pr 9 "$S9"
statuses "$S9 failure"
# The verdict belongs to the commit named, not to whatever the PR's head is now.
run 1 --pr 8 "$A8"
statuses "$A8 failure"
run 0 --pr 8 "$B8"
statuses "$B8 success"
# A fork's read-only token: the verdict is the exit code alone.
run 1 --pr 3 "$S3" --report-only
statuses ''
run 0 --pr 2 "$S2" --report-only
statuses ''
# A head that cannot be read gets error, which replaces any earlier success; never success.
run 2 --pr 7 "$S7"
statuses "$S7 error"
run 2 --pr 7 "$S7" --report-only
statuses ''
touch "$T/fx/post-fails"
run 2 --pr 1 "$S1"

# develop takes 0002 while PR 2's check runs: the first verdict is stale, the re-run flags it.
echo "touch $M/0002_other.sql" | mover
run 1 --pr 2 "$S2"
statuses "$S2 success\n$S2 failure"
has "$T/out" "develop moved past"
# A sweep that started on the older develop crosses the PR's own run on the newer one: the
# sweep's stale success is followed by its own re-check, so the newer verdict is the last.
echo '[{"number": 2, "headRefOid": "'"$S2"'"}]' > "$T/fx/prs.json"
{
  echo "touch $M/0002_other.sql"
  echo "echo '$S2 failure migration-version (PR run on the newer develop)' >> '$T/fx/statuses'"
} | mover
run 0 --open-prs
statuses "$S2 success\n$S2 failure\n$S2 failure"
# develop never holds still: no verdict stands, and error replaces the successes.
mover keep < /dev/null
run 2 --pr 1 "$S1"
statuses "$S1 success\n$S1 success\n$S1 success\n$S1 error"

# --open-prs: every open PR gets a status, migration or not; collisions do not fail the run.
jq -n --arg s1 "$S1" --arg s2 "$S2" --arg s3 "$S3" --arg s4 "$S4" --arg s5 "$S5" --arg s6 "$S6" \
  '[[1,$s1],[2,$s2],[3,$s3],[4,$s4],[5,$s5],[6,$s6]] | map({number: .[0], headRefOid: .[1]})' > "$T/fx/prs.json"
run 0 --open-prs
statuses "$S1 success\n$S2 success\n$S3 failure\n$S4 failure\n$S5 failure\n$S6 success"
# One PR that cannot be checked fails the run, gets error, and the others still get theirs.
jq -n --arg s7 "$S7" --arg s1 "$S1" '[[7,$s7],[1,$s1]] | map({number: .[0], headRefOid: .[1]})' > "$T/fx/prs.json"
run 1 --open-prs
statuses "$S7 error\n$S1 success"
echo '[]' > "$T/fx/prs.json"
run 0 --open-prs
statuses ''

exit "$FAILED"
