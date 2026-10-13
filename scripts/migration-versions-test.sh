#!/usr/bin/env bash
# Positive and negative controls for scripts/migration-versions.sh, against this repository's
# own history: the base just before #2039 merged already had 0095_member_ui_state.sql, so
# #2039's 0095_handoff_take.sql must be flagged there, and a file the base already has, or a
# free version, must not be.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FAILED=0
base=dc3ebb497 # develop after #2043 (member_ui_state), before #2039 (handoff_take)
git cat-file -e "$base^{commit}" 2>/dev/null || {
  echo "FAIL: $base is not in this clone (fetch-depth too shallow?)"
  exit 1
}

check() {
  local want="$1" input="$2" rc=0
  printf '%b' "$input" | "$HERE/migration-versions.sh" "$base" > /dev/null || rc=$?
  if [ "$rc" != "$want" ]; then
    echo "FAIL: want exit $want, got $rc for: $(printf '%b' "$input" | tr '\n' ' ')"
    FAILED=1
  fi
}

check 1 'control-plane/internal/store/migrations/0095_handoff_take.sql\n'
check 1 'control-plane/internal/store/migrations-pg/0080_handoff_take.sql\n'
check 1 'docs/a.md\ncontrol-plane/internal/store/migrations-pg/0080_handoff_take.sql\n'
check 0 'control-plane/internal/store/migrations/0095_member_ui_state.sql\n'
check 0 'control-plane/internal/store/migrations/0096_handoff_take.sql\n'
check 0 'control-plane/internal/store/migrations-pg/0081_handoff_take.sql\n'
check 0 'control-plane/session_handoff.go\n'

exit "$FAILED"
