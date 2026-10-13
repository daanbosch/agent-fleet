#!/usr/bin/env bash
# Agent Fleet — does a Persistence=retain deployment actually have its daily EFS backup?
#
# Sourced by update.sh (an upgrade preflight); run on its own it is the same check:
#
#   deploy/aws/ecs/efs-backup-check.sh --profile p --region r [--stack af-ecs-ingress]
#
# Why it exists: the vault and plan are resources of 10-data, added in 0.26.0, and no upgrade
# step redeploys that stack. A data stack stood up earlier keeps its old template for ever, so
# `Persistence=retain` reads as "backed up" while no vault, plan or recovery point exists. Nothing
# else notices. Read-only: describe/list/get calls only, it never changes a stack.
#
# The decision is a pure function (af_efs_backup_verdict) so the stub test can walk every branch
# without AWS; af_efs_backup_check gathers the facts and prints.

# af_efs_backup_verdict <persistence> <vault-name> <efs-id> <selected-resources>
# Prints one of: skip | ok | no-vault | no-plan | unknown.
#   <persistence>         the data stack's Persistence parameter; "" when it could not be read.
#   <vault-name>          the data stack's EfsBackupVaultName output ("" when absent);
#                         "UNREADABLE" when the stack could not be read at all.
#   <selected-resources>  the Resources of the plan's selections, whitespace-separated;
#                         "" when the plan has none; "UNREADABLE" when a Backup read failed.
# When in doubt this answers unknown, never ok: a missing permission must not read as protected.
af_efs_backup_verdict() {
  local persistence="$1" vault="$2" efs="$3" sel="$4"
  if [ "$vault" = UNREADABLE ] || [ -z "$persistence" ]; then echo unknown; return 0; fi
  if [ "$persistence" != retain ]; then echo skip; return 0; fi
  if [ -z "$vault" ]; then echo no-vault; return 0; fi
  if [ "$sel" = UNREADABLE ] || [ -z "$efs" ]; then echo unknown; return 0; fi
  case " $sel " in
    *"file-system/$efs "*|*"file-system/* "*) echo ok ;;
    *) echo no-plan ;;
  esac
}

# af_efs_backup_check <data-stack> <profile> <region>
# Needs the caller's AWS array (update.sh and env.sh set it). Sets AF_EFS_BACKUP_VERDICT and
# prints a warning with the fix on anything but ok / skip. Always returns 0: it advises, the
# upgrade is not blocked by it.
af_efs_backup_check() {
  local data="$1" profile="$2" region="$3"
  local persistence vault efs plan sel_ids sid got res="" rc=0
  persistence="$("${AWS[@]}" cloudformation describe-stacks --stack-name "$data" \
    --query "Stacks[0].Parameters[?ParameterKey=='Persistence'].ParameterValue" --output text 2>/dev/null)" || persistence=""
  case "$persistence" in None) persistence="" ;; esac
  vault=""; efs=""
  if [ "$persistence" = retain ]; then
    vault="$("${AWS[@]}" cloudformation describe-stacks --stack-name "$data" \
      --query "Stacks[0].Outputs[?OutputKey=='EfsBackupVaultName'].OutputValue" --output text 2>/dev/null)" || vault=UNREADABLE
    case "$vault" in None) vault="" ;; esac
    efs="$("${AWS[@]}" cloudformation describe-stacks --stack-name "$data" \
      --query "Stacks[0].Outputs[?OutputKey=='EfsId'].OutputValue" --output text 2>/dev/null)" || efs=""
    case "$efs" in None) efs="" ;; esac
  fi
  if [ -n "$vault" ] && [ "$vault" != UNREADABLE ]; then
    plan="$("${AWS[@]}" cloudformation describe-stack-resources --stack-name "$data" \
      --logical-resource-id EfsBackupPlan --query 'StackResources[0].PhysicalResourceId' \
      --output text 2>/dev/null)" || plan=""
    case "$plan" in None) plan="" ;; esac
    if [ -n "$plan" ]; then
      sel_ids="$("${AWS[@]}" backup list-backup-selections --backup-plan-id "$plan" \
        --query 'BackupSelectionsList[].SelectionId' --output text 2>/dev/null)" || rc=1
      for sid in $sel_ids; do
        [ "$sid" = None ] && continue
        got="$("${AWS[@]}" backup get-backup-selection --backup-plan-id "$plan" --selection-id "$sid" \
          --query 'BackupSelection.Resources' --output text 2>/dev/null)" || { rc=1; got=""; }
        res="$res $got"
      done
      [ "$rc" = 0 ] || res=UNREADABLE
    fi
  fi
  AF_EFS_BACKUP_VERDICT="$(af_efs_backup_verdict "$persistence" "$vault" "$efs" "$res")"
  case "$AF_EFS_BACKUP_VERDICT" in
    skip) echo "==> EFS backup: $data runs Persistence=${persistence:-delete} — no EFS backup is expected" ;;
    ok)   echo "==> EFS backup: $data has vault $vault and a plan selecting its file system" ;;
    unknown)
      {
        echo "WARNING: could not confirm the daily EFS backup of $data (a stack or AWS Backup read failed"
        echo "         — missing backup:List*/Get* or cloudformation:Describe* permission?). Check by hand:"
        echo "           aws backup list-backup-plans --query \"BackupPlansList[].BackupPlanName\" --profile $profile --region $region"
      } >&2 ;;
    no-vault|no-plan)
      {
        echo "WARNING: $data is Persistence=retain but has NO daily EFS backup ($AF_EFS_BACKUP_VERDICT)."
        echo "         The backup vault and plan were added to 10-data in 0.26.0; a data stack stood up earlier"
        echo "         never got them, and nothing else updates that stack. Every member's Claude state and"
        echo "         home on the EFS is unprotected against deletion or corruption. This upgrade does not"
        echo "         fix it — do it with a change set (README \"EFS backup and restore\", \"Turning it on\"):"
        echo "           aws cloudformation deploy --stack-name $data --template-file $(dirname "${BASH_SOURCE[0]}")/cfn/10-data.yaml \\"
        echo "             --capabilities CAPABILITY_AUTO_EXPAND CAPABILITY_IAM --no-execute-changeset \\"
        echo "             --profile $profile --region $region"
        echo "         Read the change set (describe-change-set) and execute it only if it adds the four"
        echo "         EfsBackup* resources and nothing else — never a Modify/Replacement of FileSystem or Db."
      } >&2 ;;
  esac
  return 0
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  set -euo pipefail
  PROFILE=""; REGION=""; STACK="af-ecs-ingress"
  while [ $# -gt 0 ]; do
    case "$1" in
      --profile) PROFILE="${2:?--profile needs a value}"; shift ;;
      --region)  REGION="${2:?--region needs a value}"; shift ;;
      --stack)   STACK="${2:?--stack needs a value}"; shift ;;
      -h|--help) echo "usage: efs-backup-check.sh --profile <p> --region <r> [--stack <ingress stack>]" >&2; exit 0 ;;
      *) echo "unknown arg: $1" >&2; exit 2 ;;
    esac
    shift
  done
  if [ -z "$PROFILE" ] || [ -z "$REGION" ]; then
    echo "usage: efs-backup-check.sh --profile <p> --region <r> [--stack <ingress stack>]" >&2
    exit 2
  fi
  AWS=(aws --profile "$PROFILE" --region "$REGION")
  DATA="$("${AWS[@]}" cloudformation describe-stacks --stack-name "$STACK" \
    --query "Stacks[0].Parameters[?ParameterKey=='DataStackName'].ParameterValue" --output text 2>/dev/null || true)"
  case "$DATA" in None|"") DATA=af-ecs-data ;; esac
  af_efs_backup_check "$DATA" "$PROFILE" "$REGION"
  case "$AF_EFS_BACKUP_VERDICT" in ok|skip) exit 0 ;; *) exit 1 ;; esac
fi
