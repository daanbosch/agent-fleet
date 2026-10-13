#!/usr/bin/env bash
# Migration version collisions between a PR and its base (#2046).
#
# Two PRs that each take the next free migration number are green on their own: each one's
# CI ran against a develop that did not have the other yet. Once both merge, the runner
# (store_sqlite.go) refuses to migrate and develop goes red. migration-version.yml therefore
# sets a `migration-version` commit status on every open PR into develop:
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
# run does not leave the last word, provided it is not killed between posting and that
# re-check (hence the workflow has no cancel-in-progress). When no verdict can be made (develop keeps moving, the
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

# Names travel NUL-separated and live in arrays from git to here: git quotes any name with a
# non-ASCII byte, quote or control character unless -z, and a quoted name no longer matches
# MIG_RE, so 0001_日本.sql would pass unseen although the runner keys it as version 1.
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# nul_list <array-name> <command...>: the command's NUL-separated output into the array;
# fails when the command does.
nul_list() {
  local into="$1"
  shift
  "$@" > "$TMP/list" || return 1
  mapfile -d '' -t "$into" < "$TMP/list"
}

# show <name>: the name as one log-safe token (control characters escaped).
show() {
  if [[ "$1" =~ [[:cntrl:]] ]]; then printf '%q' "$1"; else printf '%s' "$1"; fi
}

# collisions <base-ref> <path>...: fills KIND/PATH_/OTHER, one entry per problem, kind being
# base (OTHER is on <base-ref>), change (OTHER is in the same change) or bad (no readable
# version). 0 none, 1 some, 2 unknown base.
KIND=() PATH_=() OTHER=()
collisions() {
  local base="$1" p q dir v other
  local -a paths=() names=()
  shift
  KIND=() PATH_=() OTHER=()
  git rev-parse --verify -q "$base^{commit}" > /dev/null || {
    echo "unknown base $base" >&2
    return 2
  }
  for p in "$@"; do
    if [[ "$p" =~ $MIG_RE ]]; then paths+=("$p"); fi
  done
  for p in "${paths[@]}"; do
    dir="${p%/*}"
    if ! v="$(version "$p")"; then
      KIND+=(bad) PATH_+=("$p") OTHER+=("")
      continue
    fi
    nul_list names git ls-tree -z --name-only "$base" "$dir/" || return 2
    for other in "${names[@]}"; do
      [[ "$other" == "$dir/"*.sql ]] && [ "$other" != "$p" ] || continue
      if [ "$(version "$other" || true)" = "$v" ]; then
        KIND+=(base) PATH_+=("$p") OTHER+=("$other")
      fi
    done
    for q in "${paths[@]}"; do
      if [ "$q" != "$p" ] && [ "${q%/*}" = "$dir" ] && [ "$(version "$q" || true)" = "$v" ]; then
        KIND+=(change) PATH_+=("$p") OTHER+=("$q")
      fi
    done
  done
  [ "${#KIND[@]}" = 0 ]
}

# explain <base-label> <prefix>: the problems collisions found, one sentence each.
explain() {
  local i p
  for i in "${!KIND[@]}"; do
    p="$(show "${PATH_[i]}")"
    case "${KIND[i]}" in
      base) echo "$2$p: version $(version "${PATH_[i]}") is already $(show "${OTHER[i]}") on $1" ;;
      change) echo "$2$p: version $(version "${PATH_[i]}") is also $(show "${OTHER[i]}") in this change" ;;
      bad) echo "$2$p: no version prefix the runner can read (want digits, then _)" ;;
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
  local tip="$1" n="$2" sha="$3" mb rc=0 state desc p
  local -a added=()
  if have_commit "$n" "$sha" && mb="$(git merge-base "$tip" "$sha")" &&
    nul_list added git diff -z --no-renames --name-only --diff-filter=A "$mb" "$sha"; then
    collisions "$tip" "${added[@]}" || rc=$?
  else
    echo "#$n: cannot read $sha against develop $tip" >&2
    rc=2
  fi
  case "$rc" in
    0) state=success desc="no new migration's version collides with develop or within the PR" ;;
    1)
      state=failure
      explain develop "#$n: "
      p="$(show "${PATH_[0]#control-plane/internal/store/}")"
      case "${KIND[0]}" in
        base) desc="$p collides with $(show "${OTHER[0]##*/}") on develop" ;;
        change) desc="$p collides with $(show "${OTHER[0]##*/}") in this PR" ;;
        *) desc="$p has no readable version" ;;
      esac
      [ "${#KIND[@]}" -le 1 ] || desc+=" (+$((${#KIND[@]} - 1)) more)"
      desc+="; renumber it"
      ;;
    *) state=error desc="could not check this commit's migrations; see the migration-version workflow" ;;
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
    post "$sha" error "develop kept moving or could not be fetched; re-run the migration-version workflow" || true
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
mapfile -t lines
collisions "$1" "${lines[@]}" || rc=$?
explain "$1" ""
exit "$rc"
