#!/usr/bin/env bash
# opencode's npm package follows the major of the version asked for: `opencode-ai` for 1.x,
# `@opencode/cli` for 2.x. Five places spell that rule on their own (they cannot share code:
# a Dockerfile RUN, the entrypoint, two workflows' shell steps and the drift checker), so a
# sixth edit that misses one would install or compare the wrong package only on the day the
# pin crosses to 2. This runs each of them on the same versions and requires one answer.
#
#   deploy/local/opencode-pkg-test.sh
#
# Each rule is cut out of its file and evaluated, not re-implemented here; a file that no
# longer contains its rule fails the extraction (the control for "all five agreed").
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

fail() { echo "NG: $1"; exit 1; }
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# resolve <name> <version> -> the package the named place picks
resolve() { bash "$WORK/$1.sh" "$2"; }

# Dockerfile: the `case "${OPENCODE_VERSION%%.*}" in … esac;` inside the bake RUN.
rule="$(sed -n 's/^ *\(case "${OPENCODE_VERSION%%\.\*}" in .* esac\); \\$/\1/p' "$ROOT/workspace/Dockerfile")"
[ -n "$rule" ] || fail "workspace/Dockerfile: the OPENCODE_VERSION case was not found"
printf 'OPENCODE_VERSION="$1"\n%s\necho "$oc_pkg"\n' "$rule" > "$WORK/dockerfile.sh"

# entrypoint.sh: the oc_pkg() function.
rule="$(sed -n '/^oc_pkg() /p' "$ROOT/workspace/entrypoint.sh")"
[ -n "$rule" ] || fail "workspace/entrypoint.sh: oc_pkg() was not found"
printf '%s\noc_pkg "$1"\n' "$rule" > "$WORK/entrypoint.sh"

# release-gate.yml: the case on the pin inside the native live gate.
rule="$(sed -n 's/^ *\(case "${pin%%\.\*}" in .* esac\)$/\1/p' "$ROOT/.github/workflows/release-gate.yml")"
[ -n "$rule" ] || fail ".github/workflows/release-gate.yml: the pin case was not found"
printf 'pin="$1"\n%s\necho "$pkg"\n' "$rule" > "$WORK/release-gate.sh"

# setup-agent-cli: the case on the major of the spec (for `latest`, of the Dockerfile pin).
rule="$(sed -n 's/^ *\(case "${major_src%%\.\*}" in .* esac\)$/\1/p' "$ROOT/.github/actions/setup-agent-cli/action.yml")"
[ -n "$rule" ] || fail ".github/actions/setup-agent-cli/action.yml: the major_src case was not found"
printf 'major_src="$1"; pkg=opencode-ai\n%s\necho "$pkg"\n' "$rule" > "$WORK/setup-agent-cli.sh"

# cli-drift-check.sh: major_of + opencode_pkg.
{ sed -n '/^major_of() /p;/^opencode_pkg() /p' "$ROOT/deploy/local/cli-drift-check.sh"; echo 'opencode_pkg "$1"'; } > "$WORK/drift-check.sh"
grep -q '^opencode_pkg() ' "$WORK/drift-check.sh" || fail "cli-drift-check.sh: opencode_pkg() was not found"

check() { # check <version> <package>
  local place got
  for place in dockerfile entrypoint release-gate setup-agent-cli drift-check; do
    got="$(resolve "$place" "$1")"
    [ "$got" = "$2" ] || fail "$place: $1 -> '$got', want '$2'"
  done
  echo "ok  $1 -> $2"
}
check 0.9.1   opencode-ai
check 1.19.0  opencode-ai
check 1.99.0  opencode-ai
check 2.0.0   @opencode/cli
check 2.0.28  @opencode/cli
check 2.0.29  @opencode/cli
check 10.1.0  @opencode/cli

# `latest` is not a version of either line: setup-agent-cli takes the major from the Dockerfile
# pin for it. Run that step's own lines against both a 2.x and a 1.x pin (the rollback).
latest_pkg() { # latest_pkg <pin in the Dockerfile>
  local dir="$WORK/latest-$1"
  mkdir -p "$dir/workspace" "$dir/bin"
  printf 'ARG OPENCODE_VERSION=%s\n' "$1" > "$dir/workspace/Dockerfile"
  sed -n '/^        if \[ "\$AGENT" = opencode \]; then$/,/^        fi$/p' "$ROOT/.github/actions/setup-agent-cli/action.yml" |
    sed 's/^        //' > "$dir/step.sh"
  [ -s "$dir/step.sh" ] || fail "setup-agent-cli: the opencode package step was not found"
  (cd "$dir" && AGENT=opencode spec=latest pkg=opencode-ai bash -c "$(cat step.sh); echo \"\$pkg\"")
}
[ "$(latest_pkg 2.0.29)" = "@opencode/cli" ] || fail "setup-agent-cli: latest with a 2.x pin must be @opencode/cli"
[ "$(latest_pkg 1.19.0)" = "opencode-ai" ] || fail "setup-agent-cli: latest with a 1.x pin must stay opencode-ai"
echo "ok  latest -> follows the Dockerfile pin's major"
echo "OK: every place that names opencode's npm package picks it from the major alike"
