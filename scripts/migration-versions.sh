#!/usr/bin/env bash
# Migration version collisions between a PR and its base (#2046).
#
# Two PRs that each take the next free migration number are green on their own: each one's
# CI ran against a develop that did not have the other yet. Once both merge, the runner
# (store_sqlite.go) refuses to migrate and develop goes red. ci.yml's `migration-versions`
# job therefore sets a `migration-version` commit status on every open PR into develop:
# on the PR's own events, and again on every push to develop, because nothing else re-runs
# a PR's checks when develop moves. Every PR gets the status, migration or not, so branch
# protection can require it.
#
#   migration-versions.sh <base-ref> < added-paths      exit 1 and print each collision
#   migration-versions.sh --pr <n> <sha> [--report-only]
#                                                       check that head against develop and
#                                                       set the status on it
#   migration-versions.sh --open-prs                    the same for every open PR into develop
#
# A collision is a new file whose version <base-ref> already holds under another name, that
# another new file of the same change also takes, or that has no version the runner can
# read. Versions compare as the runner reads them: strconv.Atoi of the name up to the first
# "_", so 95_x.sql and 0095_y.sql collide.
#
# The PR modes judge the commit the status goes on (git diff from its merge-base with the
# develop tip), never the PR's current file list or merge ref, either of which may already
# be another commit. A verdict is posted against a freshly fetched develop and stands only
# if develop has not moved by the time it is posted; otherwise it is redone, so an older
# run can never leave the last word. When no verdict can be made (develop keeps moving, the
# head cannot be fetched) the status is `error`, which replaces an earlier success. Exit 2
# means that. The PR modes need gh, GITHUB_REPOSITORY and an `origin` remote.
set -euo pipefail

MIG_RE='^control-plane/internal/store/migrations(-pg)?/[^/]+\.sql$'

# version <file>: the version the runner keys the file by, in canonical decimal; fails when
# strconv.Atoi would refuse it.
version() {
  local f="${1##*/}" v d
  v="${f%%_*}"
  [[ "$v" =~ ^([+-]?)([0-9]{1,18})$ ]] || return 1
  d="${BASH_REMATCH[2]}"
  d="${d#"${d%%[!0]*}"}"
  [ -n "$d" ] || d=0
  if [ "${BASH_REMATCH[1]}" = - ] && [ "$d" != 0 ]; then d="-$d"; fi
  echo "$d"
}

# collisions <base-ref> < paths: one line per problem, "<kind>\t<path>\t<other path>", kind
# being base (<other> is on <base-ref>), change (<other> is in the same change) or bad.
collisions() {
  local base="$1" bad=0 p q dir v other
  local -a paths=()
  git rev-parse --verify -q "$base^{commit}" > /dev/null || {
    echo "unknown base $base" >&2
    return 2
  }
  while IFS= read -r p; do
    if [[ "$p" =~ $MIG_RE ]]; then paths+=("$p"); fi
  done
  for p in "${paths[@]}"; do
    dir="${p%/*}"
    v="$(version "$p")" || {
      printf 'bad\t%s\t\n' "$p"
      bad=1
      continue
    }
    while IFS= read -r other; do
      [[ "$other" == *.sql ]] && [ "$other" != "${p##*/}" ] || continue
      if [ "$(version "$other" || true)" = "$v" ]; then
        printf 'base\t%s\t%s\n' "$p" "$dir/$other"
        bad=1
      fi
    done < <(git ls-tree --name-only "$base" "$dir/" | sed 's|.*/||')
    for q in "${paths[@]}"; do
      if [ "$q" != "$p" ] && [ "${q%/*}" = "$dir" ] && [ "$(version "$q" || true)" = "$v" ]; then
        printf 'change\t%s\t%s\n' "$p" "$q"
        bad=1
      fi
    done
  done
  return "$bad"
}

# explain <base-label>: collisions' lines as sentences.
explain() {
  local kind p other
  while IFS=$'\t' read -r kind p other; do
    case "$kind" in
      base) echo "$p: version $(version "$p") is already $other on $1" ;;
      change) echo "$p: version $(version "$p") is also $other in this change" ;;
      bad) echo "$p: no version prefix the runner can read (want digits, then _)" ;;
    esac
  done
}

post() { # post <sha> <state> <description>
  gh api -X POST "repos/$GITHUB_REPOSITORY/statuses/$1" -f state="$2" \
    -f context=migration-version -f description="${3:0:140}" > /dev/null
}

have_commit() { # have_commit <n> <sha>: fetch a head this clone lacks (a fork's included)
  git cat-file -e "$2^{commit}" 2> /dev/null && return
  git fetch -q origin "$2" 2> /dev/null || git fetch -q origin "refs/pull/$1/head" 2> /dev/null || true
  git cat-file -e "$2^{commit}" 2> /dev/null
}

# check_pr <tip> <n> <sha> <post 0|1>: 0 clean, 1 collision, 2 could not check.
check_pr() {
  local tip="$1" n="$2" sha="$3" mb added out rc=0 state desc kind p other
  if have_commit "$n" "$sha" && mb="$(git merge-base "$tip" "$sha")" &&
    added="$(git diff --no-renames --name-only --diff-filter=A "$mb" "$sha")"; then
    out="$(collisions "$tip" <<< "$added")" || rc=$?
  else
    echo "#$n: cannot read $sha against develop $tip" >&2
    rc=2
  fi
  case "$rc" in
    0) state=success desc="no new migration's version collides with develop or within the PR" ;;
    1)
      state=failure
      explain develop <<< "$out" | sed "s/^/#$n: /"
      IFS=$'\t' read -r kind p other <<< "$out"
      case "$kind" in
        base) desc="${p#control-plane/internal/store/} collides with ${other##*/} on develop" ;;
        change) desc="${p#control-plane/internal/store/} collides with ${other##*/} in this PR" ;;
        *) desc="${p#control-plane/internal/store/} has no readable version" ;;
      esac
      [ "$(wc -l <<< "$out")" -le 1 ] || desc+=" (+$(($(wc -l <<< "$out") - 1)) more)"
      desc+="; renumber it"
      ;;
    *) state=error desc="could not check this commit's migrations; see the migration-versions job" ;;
  esac
  if [ "$4" = 1 ] && ! post "$sha" "$state" "$desc"; then
    echo "#$n: cannot set the status on $sha" >&2
    return 2
  fi
  return "$rc"
}

TIP=
# settle <n> <sha> <post 0|1>: check_pr against develop until develop holds still across the
# post. The develop-push sweep and a PR's own run race each other (separate concurrency
# groups), and only a verdict whose develop was still the tip after posting may stand.
settle() {
  local n="$1" sha="$2" rc=2 now
  for _ in 1 2 3; do
    if [ -z "$TIP" ]; then
      if ! git fetch -q origin develop; then
        echo "#$n: cannot fetch develop" >&2
        continue
      fi
      TIP="$(git rev-parse FETCH_HEAD)"
    fi
    rc=0
    check_pr "$TIP" "$n" "$sha" "$3" || rc=$?
    [ "$rc" != 2 ] || return 2
    now="$(git ls-remote origin refs/heads/develop | cut -f1)" || now=
    [ "$now" != "$TIP" ] || return "$rc"
    echo "#$n: develop moved past $TIP during the check; checking again"
    TIP=
  done
  echo "#$n: no verdict against a develop that held still" >&2
  if [ "$3" = 1 ]; then
    post "$sha" error "develop kept moving or could not be fetched; re-run the migration-versions job" || true
  fi
  return 2
}

case "${1:-}" in
  --pr)
    post=1 rc=0
    [ "${4:-}" != --report-only ] || post=0
    settle "$2" "$3" "$post" || rc=$?
    exit "$rc"
    ;;
  --open-prs)
    failed=0
    prs="$(gh pr list --repo "$GITHUB_REPOSITORY" --base develop --state open --limit 200 \
      --json number,headRefOid --jq '.[] | "\(.number) \(.headRefOid)"')"
    while read -r n sha; do
      [ -n "$n" ] || continue
      rc=0
      settle "$n" "$sha" 1 || rc=$?
      # A collision is the PR's to fix and lands in its status; only a failed check fails here.
      [ "$rc" != 2 ] || failed=1
    done <<< "$prs"
    exit "$failed"
    ;;
esac

rc=0
out="$(collisions "$1")" || rc=$?
[ -z "$out" ] || explain "$1" <<< "$out"
exit "$rc"
