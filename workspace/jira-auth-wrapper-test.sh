#!/usr/bin/env bash
# Behaviour of workspace/jira-auth-wrapper.sh against a stub `workspace-agent` (jira-cred) and a
# stub jira that records what it was started with.
#
#   workspace/jira-auth-wrapper-test.sh
#
# Tokens below are fake. What matters: which environment the real binary receives, that the
# token never shows up in argv, in a file or in the wrapper's own output, that a config for
# another Jira is left alone, and that every helper failure degrades to "jira without injected
# auth".
set -u

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WRAPPER="$HERE/jira-auth-wrapper.sh"
T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT
mkdir "$T/bin" "$T/home"

cat >"$T/bin/workspace-agent" <<'STUB'
#!/bin/sh
set +x
echo "$*" >>"$STUB_DIR/agent.calls"
[ "$1" = jira-cred ] || exit 2
[ -z "${JIRA_API_TOKEN-}" ] || echo leaked >"$STUB_DIR/agent.leak"
[ -f "$STUB_DIR/agent.out" ] || exit 1
cat "$STUB_DIR/agent.out"
STUB
# The "real" jira: one line per interesting env var plus its argv, one element per line.
cat >"$T/bin/jira-real" <<'STUB'
#!/bin/sh
set +x
{
  echo "TOKEN=${JIRA_API_TOKEN-<unset>}"
  echo "SERVER=${JIRA_SERVER-<unset>}"
  echo "AUTH=${JIRA_AUTH_TYPE-<unset>}"
  echo "LOGIN=${JIRA_LOGIN-<unset>}"
  echo "INSTALLATION=${JIRA_INSTALLATION-<unset>}"
  echo "CONFIG=${JIRA_CONFIG_FILE-<unset>}"
  echo "DEBUG=${JIRA_DEBUG-<unset>}"
  for a in "$@"; do echo "ARG=$a"; done
} >"$STUB_DIR/jira.seen"
exit "${STUB_JIRA_EXIT:-0}"
STUB
chmod +x "$T/bin/"*

fails=0
fail() { echo "FAIL: $*" >&2; fails=$((fails + 1)); }

# run <env assignments…> -- <jira args…>: a clean slate, then the wrapper. stdout/stderr land in
# $T/out and $T/err, the exit status in $rc, what the stub saw in $T/jira.seen.
run() {
  local envs=()
  while [ "$1" != -- ]; do envs+=("$1"); shift; done
  shift
  rm -f "$T/jira.seen" "$T/agent.calls" "$T/agent.leak"
  env -i PATH="$T/bin:/usr/bin:/bin" HOME="$T/home" STUB_DIR="$T" AF_JIRA_REAL="$T/bin/jira-real" \
    "${envs[@]}" bash "$WRAPPER" "$@" >"$T/out" 2>"$T/err"
  rc=$?
}
seen() { sed -n "s/^$1=//p" "$T/jira.seen"; }
expect() { # expect <label> <var> <want>
  local got
  got="$(seen "$2")"
  [ "$got" = "$3" ] || fail "$1: $2 = '$got', want '$3'"
}
no_leak() { # no_leak <label> <secret>: not in argv, not in any file under HOME, not in output
  if grep -q "^ARG=.*$2" "$T/jira.seen" 2>/dev/null; then fail "$1: secret in argv"; fi
  if grep -rq "$2" "$T/home" 2>/dev/null; then fail "$1: secret written under HOME"; fi
  if grep -q "$2" "$T/out" "$T/err" 2>/dev/null; then fail "$1: secret in wrapper output"; fi
}

SITE=https://acme.atlassian.net
ROUTE=https://api.atlassian.com/ex/jira/cid-1
DEFCFG="$T/home/.config/.jira/.config.yml"
MANAGED="$T/home/.config/agent-fleet/jira"
GEN="$T/home/.cache/agent-fleet/jira/config.yml"
oauth() { printf 'site=%s\nserver=%s\nauth=bearer\nlogin=me@example.com\ntoken=fake-oauth-tok\n' "$SITE" "$ROUTE" >"$T/agent.out"; }
apitok() { printf 'site=%s\nserver=%s\nauth=basic\nlogin=me@example.com\ntoken=fake-api-tok\n' "$SITE" "$SITE" >"$T/agent.out"; }
reset() { rm -f "$T/agent.out"; rm -rf "$T/home/.config" "$T/home/.cache" "$T/other.yml"; }
config() { mkdir -p "${1%/*}"; printf '%s\n' "$2" >"$1"; }

# 1. OAuth connection, no config → bearer on the api.atlassian.com route, through a generated
#    config that holds no secret, argv intact.
reset
oauth
run -- issue list -q "status = 'In Progress'"
expect oauth TOKEN fake-oauth-tok
expect oauth SERVER "$ROUTE"
expect oauth AUTH bearer
expect oauth LOGIN me@example.com
expect oauth INSTALLATION Cloud
expect oauth CONFIG "$GEN"
[ -s "$GEN" ] || fail "oauth: no generated config"
grep -q '^installation: Cloud$' "$GEN" || fail "oauth: generated config lacks the installation"
[ "$(grep -c '^ARG=' "$T/jira.seen")" = 4 ] || fail "oauth: argv not passed through intact"
grep -q "^ARG=status = 'In Progress'$" "$T/jira.seen" || fail "oauth: argument with spaces was split"
no_leak oauth fake-oauth-tok

# 2. API-token connection → basic against the site with the account email.
reset
apitok
run -- issue view KEY-1
expect api TOKEN fake-api-tok
expect api SERVER "$SITE"
expect api AUTH basic
expect api LOGIN me@example.com
no_leak api fake-api-tok

# 3. An OAuth account that hides its email: no login is exported, not even a stale one.
reset
printf 'site=%s\nserver=%s\nauth=bearer\ntoken=fake-oauth-tok\n' "$SITE" "$ROUTE" >"$T/agent.out"
run JIRA_LOGIN=stale@example.com -- issue list
expect nologin TOKEN fake-oauth-tok
expect nologin LOGIN '<unset>'

# 4. The helper fails or answers without a token → jira still runs, nothing injected, exit
#    status kept.
reset
run STUB_JIRA_EXIT=7 -- issue list
[ "$rc" = 7 ] || fail "helper failure: exit status $rc, want 7 from jira"
expect nohelper TOKEN '<unset>'
expect nohelper SERVER '<unset>'
expect nohelper CONFIG "$GEN" # secret-free, and jira refuses to start without one
printf 'site=%s\nserver=%s\nauth=bearer\n' "$SITE" "$ROUTE" >"$T/agent.out"
run -- issue list
expect notoken TOKEN '<unset>'

# 5. Explicit JIRA_API_TOKEN is never replaced, and the helper is not even consulted.
reset
oauth
run JIRA_API_TOKEN=mine -- issue list
expect explicit TOKEN mine
expect explicit SERVER '<unset>'
[ ! -f "$T/agent.calls" ] || fail "explicit JIRA_API_TOKEN: helper was consulted"

# 6. A config under the managed directory (where `jira init` through the wrapper writes) gets
#    the credential, however -c is spelt; the default managed config is picked up with no -c.
reset
oauth
config "$MANAGED/config.yml" "installation: Cloud"
run -- sprint list
expect managed-default TOKEN fake-oauth-tok
expect managed-default CONFIG "$MANAGED/config.yml"
config "$MANAGED/board-b.yml" "installation: Cloud"
for form in "-c $MANAGED/board-b.yml" "-c$MANAGED/board-b.yml" "-c=$MANAGED/board-b.yml" "--config=$MANAGED/board-b.yml"; do
  # shellcheck disable=SC2086 # the form is split on purpose
  run -- $form sprint list
  expect "managed $form" TOKEN fake-oauth-tok
done
run JIRA_CONFIG_FILE="$MANAGED/board-b.yml" -- sprint list
expect managed-env TOKEN fake-oauth-tok
run -- -c "$MANAGED/../../agent-fleet/jira/board-b.yml" sprint list
expect managed-dotdot TOKEN fake-oauth-tok

# 7. Any other config is the user's setup, whatever its contents say: nothing injected, in every
#    -c spelling, via JIRA_CONFIG_FILE, at jira's own default location, or one that only looks like
#    the managed directory.
reset
oauth
config "$T/other.yml" '"server": https://jira.example.test'
for form in "-c $T/other.yml" "-c$T/other.yml" "-c=$T/other.yml" "--config $T/other.yml" "--config=$T/other.yml"; do
  # shellcheck disable=SC2086
  run -- $form issue list
  expect "other $form" TOKEN '<unset>'
done
run -- issue list -c "$MANAGED/board-b.yml" -c "$T/other.yml"
expect other-last-wins TOKEN '<unset>'
run JIRA_CONFIG_FILE="$T/other.yml" -- issue list
expect other-env TOKEN '<unset>'
config "$DEFCFG" "server: $SITE"
run -- issue list
expect jira-default TOKEN '<unset>'
rm -f "$DEFCFG"
config "$MANAGED-evil/config.yml" "x: 1"
run -- -c "$MANAGED-evil/config.yml" issue list
expect lookalike TOKEN '<unset>'
run JIRA_SERVER=https://jira.example.test -- issue list
expect explicit-server SERVER https://jira.example.test
expect explicit-server TOKEN '<unset>'

# 8. JIRA_CONFIG_FILE naming a managed file that does not exist yet is not replaced by the
#    generated one.
reset
oauth
run JIRA_CONFIG_FILE="$MANAGED/missing.yml" -- issue list
expect missingcfg CONFIG "$MANAGED/missing.yml"
[ ! -e "$GEN" ] || fail "missingcfg: generated a config the user did not ask for"

# 9. `jira init` (and every alias of it) writes the managed config and gets the connection as
#    flags, so it never prompts for them.
reset
oauth
run -- init --project ABC --board "ABC board" --force
expect init TOKEN fake-oauth-tok
expect init CONFIG "$MANAGED/config.yml"
want_args="init
--project
ABC
--board
ABC board
--force
--installation
cloud
--server
$ROUTE
--auth-type
bearer
--login
me@example.com"
[ "$(seen ARG)" = "$want_args" ] || fail "init: argv = $(seen ARG | tr '\n' ' ')"
no_leak init fake-oauth-tok
for alias in initialize configure config setup; do
  run -- "$alias" --project ABC
  expect "init alias $alias" CONFIG "$MANAGED/config.yml"
  seen ARG | grep -qx -- --server || fail "init alias $alias: connection flags not added"
done
run -- init -c "$MANAGED/board-b.yml" --project ABC
expect init-managed-c TOKEN fake-oauth-tok
expect init-managed-c CONFIG '<unset>'

# 9b. …even when jira's own default config exists (init writes the managed one instead).
config "$DEFCFG" "server: https://jira.example.test"
run -- init --project ABC --force
expect init-existing CONFIG "$MANAGED/config.yml"
rm -f "$DEFCFG"

# 9c. An OAuth account with no email still gets past init's login prompt.
rm -f "$T/agent.out"
printf 'site=%s\nserver=%s\nauth=bearer\ntoken=fake-oauth-tok\n' "$SITE" "$ROUTE" >"$T/agent.out"
run -- init --project ABC
seen ARG | grep -qx oauth || fail "init without email: no placeholder login"

# 9d. Any init (or alias) naming its own server or login, or a config outside the managed
#     directory, configures another Jira: untouched.
reset
oauth
for alias in init configure setup; do
  run -- "$alias" --server https://jira.example.test --installation local
  expect "init-own $alias" TOKEN '<unset>'
  [ "$(grep -c '^ARG=' "$T/jira.seen")" = 5 ] || fail "init-own $alias: flags were added"
done
run -- configure --server=https://jira.example.test --force
expect init-own-eq TOKEN '<unset>'
run -- init -c "$T/other.yml" --project ABC
expect init-other-c TOKEN '<unset>'

# 9e. A debug run dumps the Authorization header, so it never gets the token: --debug, JIRA_DEBUG,
#     or debug: true in the managed config.
reset
oauth
run -- --debug issue view KEY-1
expect debug-flag TOKEN '<unset>'
[ ! -f "$T/agent.calls" ] || fail "debug-flag: helper was consulted"
grep -q 'Authorization' "$T/err" || fail "debug-flag: no explanation on stderr"
run -- issue view KEY-1 --debug=true
expect debug-eq TOKEN '<unset>'
run JIRA_DEBUG=1 -- issue view KEY-1
expect debug-env TOKEN '<unset>'
run JIRA_DEBUG=false -- issue view KEY-1
expect debug-env-off TOKEN fake-oauth-tok
# A config's own debug cannot be parsed reliably, so every injected run forces it off instead.
expect debug-env-off DEBUG false
config "$MANAGED/config.yml" "{installation: Cloud, debug: !!bool true}"
run -- issue view KEY-1
expect debug-config TOKEN fake-oauth-tok
expect debug-config DEBUG false

# 10. `jira version` skips the helper.
reset
oauth
run -- version
[ ! -f "$T/agent.calls" ] || fail "version: helper was consulted"
expect version TOKEN '<unset>'

# 11. An inherited xtrace must not echo the token.
reset
oauth
run SHELLOPTS=xtrace -- issue list
expect xtrace TOKEN fake-oauth-tok
no_leak xtrace fake-oauth-tok

# 11b. An inherited allexport must not export the token (or the helper's answer) to the helpers
#      the wrapper runs, nor may a pre-exported variable of the same name.
reset
oauth
cat >"$T/bin/sed" <<'STUB'
#!/bin/sh
env | grep -q fake-oauth-tok && echo leaked >"$STUB_DIR/sed.leak"
exec /bin/sed "$@"
STUB
chmod +x "$T/bin/sed"
rm -f "$T/sed.leak"
run SHELLOPTS=allexport -- issue list
[ ! -f "$T/sed.leak" ] || fail "allexport: the token reached a helper's environment"
expect allexport TOKEN fake-oauth-tok
rm -f "$T/sed.leak"
run tok=preexported cred=preexported -- issue list
[ ! -f "$T/sed.leak" ] || fail "pre-exported names: the token reached a helper's environment"
rm -f "$T/bin/sed"

# 12. The helper runs before the export, so it never sees a token in its environment.
reset
oauth
run -- issue list
[ ! -f "$T/agent.leak" ] || fail "JIRA_API_TOKEN visible to the helper"

if [ "$fails" != 0 ]; then
  echo "jira-auth-wrapper-test: $fails failure(s)" >&2
  exit 1
fi
echo "jira-auth-wrapper-test: ok"
