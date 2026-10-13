#!/usr/bin/env bash
# Migration version collisions between a PR and its base (#2046).
#
# Two PRs that each take the next free migration number are green on their own: each one's
# CI ran against a develop that did not have the other yet. Once both merge, the runner
# (store_sqlite.go) refuses to migrate and develop goes red. ci.yml's `migration-versions`
# job therefore sets a `migration-version` commit status on every open PR into develop:
# on the PR's own events against the develop tip of that moment, and again on every push
# to develop, because nothing else re-runs a PR's checks when develop moves. Every PR gets
# the status, migration or not, so branch protection can require it.
#
#   migration-versions.sh <base-ref> < added-paths   exit 1 and print each collision
#   migration-versions.sh --pr <number> [<sha>]      check one PR against origin/develop;
#                                                    with <sha>, set the status on it
#   migration-versions.sh --open-prs <base-ref>      set the status on every open PR into
#                                                    develop
#
# A collision is a new file whose version <base-ref> already holds under another name, or
# that another new file of the same change also takes. Exit 2 means the check could not be
# made (unknown base, API failure); no success status is ever posted then. The PR modes
# need gh and GITHUB_REPOSITORY.
set -euo pipefail

migration_path() {
  case "$1" in
    control-plane/internal/store/migrations/*.sql | control-plane/internal/store/migrations-pg/*.sql) return 0 ;;
  esac
  return 1
}

version() {
  local f="${1##*/}"
  echo "${f%%_*}"
}

# collisions <base-ref> < paths: one line per collision, "<path>\t<other path>\t<where>",
# where <where> is <base-ref>, or empty when <other path> is in the same change.
collisions() {
  local base="$1" bad=0 p q dir f other
  local -a paths=()
  git rev-parse --verify -q "$base^{commit}" > /dev/null || {
    echo "unknown base $base" >&2
    return 2
  }
  while IFS= read -r p; do
    if migration_path "$p"; then paths+=("$p"); fi
  done
  for p in "${paths[@]}"; do
    dir="${p%/*}" f="${p##*/}"
    while IFS= read -r other; do
      other="${other##*/}"
      if [ "$other" != "$f" ] && [ "${other%%_*}" = "${f%%_*}" ]; then
        printf '%s\t%s\t%s\n' "$p" "$dir/$other" "$base"
        bad=1
      fi
    done < <(git ls-tree --name-only "$base" "$dir/")
    for q in "${paths[@]}"; do
      if [ "$q" != "$p" ] && [ "${q%/*}" = "$dir" ] && [ "$(version "$q")" = "${f%%_*}" ]; then
        printf '%s\t%s\t\n' "$p" "$q"
        bad=1
      fi
    done
  done
  return "$bad"
}

explain() {
  local p other where
  while IFS=$'\t' read -r p other where; do
    if [ -n "$where" ]; then
      echo "$p: version $(version "$p") is already $other on $where"
    else
      echo "$p: version $(version "$p") is also $other in this change"
    fi
  done
}

# The files a PR adds, renames or copies (merge-base..head, as GitHub lists them), never
# its merge ref: that one's base is a snapshot of develop from the PR's last push.
pr_migrations() {
  local files p
  files="$(gh api --paginate "repos/$GITHUB_REPOSITORY/pulls/$1/files" \
    --jq '.[] | select(.status == "added" or .status == "renamed" or .status == "copied") | .filename')" || return 1
  while IFS= read -r p; do
    if migration_path "$p"; then echo "$p"; fi
  done <<< "$files"
}

# check_pr <base-ref> <number> [<sha>]: 0 clean, 1 collision, 2 could not check.
check_pr() {
  local base="$1" n="$2" sha="${3:-}" added out rc=0 state desc p other where
  added="$(pr_migrations "$n")" || {
    echo "#$n: cannot list its files" >&2
    return 2
  }
  out="$(collisions "$base" <<< "$added")" || rc=$?
  case "$rc" in
    0) state=success desc="no new migration's version collides with develop or within the PR" ;;
    1)
      state=failure
      explain <<< "$out" | sed "s/^/#$n: /"
      IFS=$'\t' read -r p other where <<< "$out"
      desc="${p#control-plane/internal/store/} collides with ${other##*/}"
      if [ -n "$where" ]; then desc+=" on develop"; else desc+=" in this PR"; fi
      [ "$(wc -l <<< "$out")" -le 1 ] || desc+=" (+$(($(wc -l <<< "$out") - 1)) more)"
      desc+="; renumber it"
      ;;
    *) return 2 ;;
  esac
  [ -n "$sha" ] || return "$rc"
  gh api -X POST "repos/$GITHUB_REPOSITORY/statuses/$sha" -f state="$state" \
    -f context=migration-version -f description="${desc:0:140}" > /dev/null || {
    echo "#$n: cannot set the status on $sha" >&2
    return 2
  }
  return "$rc"
}

case "${1:-}" in
  --pr)
    n="$2" sha="${3:-}" rc=0
    # Re-run when develop moved while this ran: the develop-push re-check may already have
    # posted on this head, and a verdict against the older tip must not be the last word.
    for _ in 1 2 3; do
      git fetch -q origin develop
      tip="$(git rev-parse FETCH_HEAD)"
      rc=0
      check_pr "$tip" "$n" "$sha" || rc=$?
      [ "$rc" != 2 ] || exit 2
      [ "$(git ls-remote origin refs/heads/develop | cut -f1)" != "$tip" ] || break
      echo "develop moved past $tip during the check; checking again"
    done
    exit "$rc"
    ;;
  --open-prs)
    base="$2" failed=0
    prs="$(gh pr list --repo "$GITHUB_REPOSITORY" --base develop --state open --limit 200 \
      --json number,headRefOid --jq '.[] | "\(.number) \(.headRefOid)"')"
    while read -r n sha; do
      [ -n "$n" ] || continue
      rc=0
      check_pr "$base" "$n" "$sha" || rc=$?
      # A collision is the PR's to fix and lands in its status; only a failed check fails here.
      [ "$rc" != 2 ] || failed=1
    done <<< "$prs"
    exit "$failed"
    ;;
esac

out="$(collisions "$1")" || rc=$?
[ -z "$out" ] || explain <<< "$out"
exit "${rc:-0}"
