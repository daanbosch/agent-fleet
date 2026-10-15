#!/usr/bin/env bash
# Jira CLI (jira, ankitpokhrel/jira-cli) transparent-auth wrapper — the Jira counterpart of
# bkt-auth-wrapper.sh.
#
# jira reads its token from JIRA_API_TOKEN, a .netrc or an OS keychain, none of which can
# follow a one-hour OAuth token. This sits in front of the real binary (/usr/local/libexec/jira),
# asks the Agent for the Connections credential on every call (`workspace-agent jira-cred`, which
# renews an OAuth token about to expire) and hands it over through jira's environment, which
# overrides the config file key by key (JIRA_SERVER, JIRA_LOGIN, JIRA_AUTH_TYPE,
# JIRA_INSTALLATION). docs/build/08 §8.4.2.
#
# - An explicit JIRA_API_TOKEN is the user's own setup: exec unchanged.
# - A config file (the one -c/--config, JIRA_CONFIG_FILE or the default location names) whose
#   `server:` is another Jira is the user's too: injecting would send this connection's token
#   there. A config whose server is the connected site, or its api.atlassian.com route, is the
#   one `jira init` writes through this wrapper, and gets the credential.
# - No config at all: jira refuses to start without one, so point JIRA_CONFIG_FILE at a minimal
#   one holding no secret. `jira init` instead gets the connection's server, login, installation
#   and auth type as flags, so `jira init --project KEY --board NAME` runs without prompts.
# - Fails closed: any helper error leaves jira running with no injected auth.
# - The token only ever lives in this shell's variables and, at the very end, in the exec'd
#   jira's environment: never argv, never a file, never printed. xtrace is switched off first
#   because an inherited SHELLOPTS=xtrace would echo every assignment below.
set +x

real="${AF_JIRA_REAL:-/usr/local/libexec/jira}"

# `jira version` needs no credential. Skipping the helper keeps the Toolchains probe (which
# runs this wrapper) from taking the store lock or renewing an OAuth token over the network.
if [ "$#" -eq 1 ] && [ "$1" = version ]; then
  exec "$real" "$@"
fi

if [ -n "${JIRA_API_TOKEN:-}" ]; then
  exec "$real" "$@"
fi

# The config jira will read and the subcommand, from argv. Only the flags that take a value
# before the subcommand matter for finding it.
cfg="" sub="" own_init=""
prev=""
for a in "$@"; do
  case "$prev" in
    -c | --config) cfg="$a"; prev=""; continue ;;
    -p | --project) prev=""; continue ;;
  esac
  case "$a" in
    -c | --config | -p | --project) prev="$a" ;;
    --config=*) cfg="${a#--config=}" ;;
    --server | --server=* | --login | --login=* | --installation | --installation=* | --auth-type | --auth-type=*)
      own_init=1 ;;
    -*) ;;
    *) [ -z "$sub" ] && sub="$a" ;;
  esac
done
explicit_cfg="${cfg:-${JIRA_CONFIG_FILE:-}}"
if [ -z "$explicit_cfg" ]; then
  d="${XDG_CONFIG_HOME:-${HOME:-/nonexistent}/.config}/.jira"
  cfg="$d/.config.yml"
  [ -e "$cfg" ] || [ ! -e "$d/.config.yaml" ] || cfg="$d/.config.yaml"
else
  cfg="$explicit_cfg"
fi

# `jira init` with any connection flag of the user's own is configuring another Jira.
if [ "$sub" = init ] && [ -n "$own_init" ]; then
  exec "$real" "$@"
fi

# First value of a `key=value` line in the helper's answer on stdin.
field() { sed -n "s/^$1=//;T;p;q"; }

cred="$(workspace-agent jira-cred 2>/dev/null)" || cred=""
tok="$(printf '%s\n' "$cred" | field token)"
site="$(printf '%s\n' "$cred" | field site)"
server="$(printf '%s\n' "$cred" | field server)"
auth="$(printf '%s\n' "$cred" | field auth)"
login="$(printf '%s\n' "$cred" | field login)"
if [ -z "$tok" ] || [ -z "$server" ] || [ -z "$auth" ]; then
  exec "$real" "$@"
fi

if [ -e "$cfg" ] && [ "$sub" != init ]; then
  have="$(sed -n "s/^server:[[:space:]]*//p" "$cfg" 2>/dev/null | head -n1 | tr -d "\"'\r" | sed 's/[[:space:]]*$//; s:/*$::')"
  if [ -n "$have" ] && [ "$have" != "$site" ] && [ "$have" != "$server" ]; then
    exec "$real" "$@"
  fi
elif [ "$sub" = init ]; then
  # A bearer login is replaced by what /myself reports, so the placeholder only gets past
  # init's prompt for an empty one.
  set -- "$@" --installation cloud --server "$server" --auth-type "$auth" --login "${login:-oauth}"
elif [ -z "$explicit_cfg" ]; then
  gen="${XDG_CACHE_HOME:-${HOME:-/nonexistent}/.cache}/agent-fleet/jira/config.yml"
  if [ ! -s "$gen" ]; then
    mkdir -p "${gen%/*}" 2>/dev/null &&
      printf '# Written by the jira wrapper (no secrets). Run `jira init --project KEY --board NAME`\n# for a default project, board, issue types and epic fields.\ninstallation: Cloud\n' >"$gen" 2>/dev/null
  fi
  [ -s "$gen" ] && export JIRA_CONFIG_FILE="$gen"
fi

export JIRA_API_TOKEN="$tok" JIRA_SERVER="$server" JIRA_AUTH_TYPE="$auth" JIRA_INSTALLATION=Cloud
if [ -n "$login" ]; then export JIRA_LOGIN="$login"; else unset JIRA_LOGIN; fi
exec "$real" "$@"
