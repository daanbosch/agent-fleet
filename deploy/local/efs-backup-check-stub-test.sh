#!/usr/bin/env bash
# efs-backup-check.sh (the upgrade preflight behind update.sh), against a stubbed `aws`.
#
#   deploy/local/efs-backup-check-stub-test.sh
#
# Pinned: the DECISION. A data stack stood up before 0.26.0 has no EFS backup vault or plan and
# nothing else says so (#2063), so the cases that matter are the ones that must WARN
# (no-vault, no-plan) and the ones that must not claim protection they could not verify
# (unknown). The "ok" and "skip" cases keep the warning honest: a check that always warns is
# one nobody reads.
# shellcheck disable=SC2016  # case 7 greps for literal source text, not expansions
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
CHECK="$ROOT/deploy/aws/ecs/efs-backup-check.sh"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
STUB="$WORK/bin"; LOG="$WORK/calls.log"
mkdir -p "$STUB"

# Knobs (environment): STUB_PERSISTENCE, STUB_VAULT, STUB_EFS, STUB_PLAN, STUB_SELECTIONS
# (ids), STUB_RESOURCES (what get-backup-selection returns), STUB_BACKUP_DENIED=1.
cat > "$STUB/aws" <<'FAKE'
#!/usr/bin/env bash
echo "aws $*" >> "$STUB_LOG"
case "$*" in
  *"describe-stacks --stack-name t-ingress"*DataStackName*) echo "t-data" ;;
  *"describe-stacks --stack-name t-data"*Persistence*) echo "${STUB_PERSISTENCE:-retain}" ;;
  *"describe-stacks --stack-name t-data"*EfsBackupVaultName*) echo "${STUB_VAULT:-None}" ;;
  *"describe-stacks --stack-name t-data"*"'EfsId'"*) echo "${STUB_EFS:-fs-0abc}" ;;
  *"describe-stack-resources"*) echo "${STUB_PLAN:-None}" ;;
  *"backup list-backup-selections"*)
    [ -z "${STUB_BACKUP_DENIED:-}" ] || { echo "AccessDenied" >&2; exit 254; }
    echo "${STUB_SELECTIONS:-None}" ;;
  *"backup get-backup-selection"*)
    [ -z "${STUB_BACKUP_DENIED:-}" ] || { echo "AccessDenied" >&2; exit 254; }
    echo "${STUB_RESOURCES:-}" ;;
esac
exit 0
FAKE
chmod +x "$STUB/aws"
export PATH="$STUB:$PATH" STUB_LOG="$LOG"

fail() { echo "NG: $1"; echo "--- out ---"; cat "$WORK/out" 2>/dev/null; echo "--- log ---"; cat "$LOG"; exit 1; }
says() { grep -qF -- "$1" "$WORK/out" || fail "not printed: $1"; }
saysnt() { ! grep -qF -- "$1" "$WORK/out" || fail "must not be printed: $1"; }
# run <expected exit> [env assignments via the environment]
check() {
  local want="$1" got=0
  : > "$LOG"
  "$CHECK" --profile p --region ap-northeast-1 --stack t-ingress > "$WORK/out" 2>&1 || got=$?
  [ "$got" = "$want" ] || fail "exit $got, wanted $want"
  # Read-only: no stack or backup write may ever be issued.
  ! grep -Eq 'aws (cloudformation (deploy|create|update|execute|delete)|backup (create|update|delete|put|start))' "$LOG" \
    || fail "a write call was issued"
}
ARN="arn:aws:elasticfilesystem:ap-northeast-1:111122223333:file-system/fs-0abc"

echo "== verdict: the pure decision =="
# shellcheck source=deploy/aws/ecs/efs-backup-check.sh
. "$CHECK"
v() { [ "$(af_efs_backup_verdict "$@")" = "$WANT" ] || fail "verdict($*) = $(af_efs_backup_verdict "$@"), wanted $WANT"; }
WANT=skip;     v delete "" fs-1 ""
WANT=no-vault; v retain "" fs-1 ""
WANT=no-plan;  v retain vault fs-1 ""
WANT=no-plan;  v retain vault fs-1 "arn:x:file-system/fs-other"
WANT=ok;       v retain vault fs-1 "arn:x:file-system/fs-1"
WANT=ok;       v retain vault fs-1 "arn:x:file-system/fs-other arn:x:file-system/fs-1"
WANT=ok;       v retain vault fs-1 "arn:aws:elasticfilesystem:*:*:file-system/*"
# a prefix of the id is not the id
WANT=no-plan;  v retain vault fs-1 "arn:x:file-system/fs-12"
WANT=unknown;  v retain vault fs-1 UNREADABLE
WANT=unknown;  v retain UNREADABLE fs-1 ""
WANT=unknown;  v "" "" "" ""

echo "== case 1: Persistence=delete -- nothing expected, nothing said =="
STUB_PERSISTENCE=delete check 0
says "no EFS backup is expected"; saysnt "WARNING"

echo "== case 2: retain, pre-0.26.0 data stack (no vault output) -- warns with the fix =="
STUB_VAULT=None check 1
says "NO daily EFS backup (no-vault)"
says "--no-execute-changeset"
says "never a Modify/Replacement of FileSystem or Db"
saysnt "execute-change-set"

echo "== case 3: retain, vault but the plan selects nothing -- warns =="
STUB_VAULT=t-data-efs-1a2b3c4d STUB_PLAN=plan-1 STUB_SELECTIONS=None check 1
says "(no-plan)"

echo "== case 4: retain, plan selects another file system -- warns =="
STUB_VAULT=v STUB_PLAN=plan-1 STUB_SELECTIONS=sel-1 STUB_RESOURCES="arn:x:file-system/fs-other" check 1
says "(no-plan)"

echo "== case 5: retain, plan selects this EFS -- ok =="
STUB_VAULT=v STUB_PLAN=plan-1 STUB_SELECTIONS=sel-1 STUB_RESOURCES="$ARN" check 0
says "has vault v and a plan selecting"; saysnt "WARNING"

echo "== case 6: AWS Backup not readable -- never reads as protected =="
STUB_VAULT=v STUB_PLAN=plan-1 STUB_SELECTIONS=sel-1 STUB_BACKUP_DENIED=1 check 1
says "could not confirm"; saysnt "has vault"

echo "== case 7: update.sh runs the preflight, and reminds at the end =="
UPD="$ROOT/deploy/aws/ecs/update.sh"
grep -q '^\. "\$HERE/efs-backup-check.sh"$' "$UPD" || fail "update.sh does not source efs-backup-check.sh"
grep -q 'af_efs_backup_check "\$DATA_STACK"' "$UPD" || fail "update.sh does not call af_efs_backup_check"
grep -q 'REMINDER: \$DATA_STACK' "$UPD" || fail "update.sh has no end-of-run reminder"

echo "OK: the EFS backup preflight decides correctly"
