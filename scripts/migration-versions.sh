#!/usr/bin/env bash
# Migration version collisions between a PR and its base (#2046).
#
# Two PRs that each take the next free migration number are green on their own: each one's
# CI ran against a develop that did not have the other yet. Once both merge, the runner
# (store_sqlite.go) refuses to migrate and develop goes red. Nothing re-runs a PR's checks
# when develop moves, so ci.yml's `migration-versions` job runs this on every push to
# develop and marks each open PR whose new migration's version develop now holds.
#
#   migration-versions.sh <base-ref> < added-paths   exit 1 and print each collision
#   migration-versions.sh --open-prs <base-ref>      set the migration-version commit status
#                                                    on every open PR into develop that adds
#                                                    or renames a migration (needs gh and
#                                                    GITHUB_REPOSITORY)
set -euo pipefail

collisions() {
  local base="$1" bad=0 p dir f other
  while IFS= read -r p; do
    case "$p" in
      control-plane/internal/store/migrations/*.sql | control-plane/internal/store/migrations-pg/*.sql) ;;
      *) continue ;;
    esac
    dir="${p%/*}" f="${p##*/}"
    while IFS= read -r other; do
      other="${other##*/}"
      if [ "$other" != "$f" ] && [ "${other%%_*}" = "${f%%_*}" ]; then
        echo "$p: version ${f%%_*} is already $dir/$other on $base"
        bad=1
      fi
    done < <(git ls-tree --name-only "$base" "$dir/")
  done
  return "$bad"
}

if [ "${1:-}" != --open-prs ]; then
  collisions "$1"
  exit
fi

base="$2" rc=0
gh pr list --repo "$GITHUB_REPOSITORY" --base develop --state open --limit 200 \
  --json number,headRefOid --jq '.[] | "\(.number) \(.headRefOid)"' |
  while read -r n sha; do
    added="$(gh api --paginate "repos/$GITHUB_REPOSITORY/pulls/$n/files" \
      --jq '.[] | select(.status == "added" or .status == "renamed") | .filename' | grep '^control-plane/internal/store/migrations' || true)"
    [ -n "$added" ] || continue
    if out="$(collisions "$base" <<<"$added")"; then
      state=success desc="no migration version collides with develop"
    else
      echo "#$n: $out"
      state=failure desc="a new migration's version is taken on develop; renumber it"
    fi
    gh api -X POST "repos/$GITHUB_REPOSITORY/statuses/$sha" -f state="$state" \
      -f context=migration-version -f description="$desc" > /dev/null
  done || rc=$?
exit "$rc"
