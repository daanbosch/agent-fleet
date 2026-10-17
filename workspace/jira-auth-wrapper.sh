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
# Whose config is it? Decided by WHERE the config is, never by reading it: a config's server
# can be spelt in more ways than a shell can parse (quoted keys, comments, other formats), and a
# wrong guess sends this connection's token to another Jira.
# - The credential is injected only for a config under $managed (`jira init` run through this
#   wrapper writes there) or, with no config named at all, for a generated one holding no secret.
# - Any other config — jira's own default location, or -c / JIRA_CONFIG_FILE elsewhere — and an
#   explicit JIRA_API_TOKEN or JIRA_SERVER are the user's own setup: exec unchanged.
# - A misread argv can only make a config look like someone else's, which runs jira without the
#   token. That is the direction to fail in.
# - A debug run is never given the token: jira --debug dumps every request, Authorization included.
#   A config's own `debug:` cannot be read reliably (YAML tags, flow style, JSON), so an injected
#   run forces JIRA_DEBUG=false, which jira ranks above the config and below the flag.
# - Fails closed: any helper error leaves jira running with no injected auth.
# - The token only ever lives in this shell's variables and, at the very end, in the exec'd
#   jira's environment: never argv, never a file, never printed. xtrace and allexport are
#   switched off first, and the variables are unset so none inherits an export attribute:
#   otherwise the assignments below would be echoed, or exported to every helper.
set +xa
unset cred tok server auth login cfg sub own_init debug prev conf managed gen

real="${AF_JIRA_REAL:-/usr/local/libexec/jira}"

# `jira version` needs no credential. Skipping the helper keeps the Toolchains probe (which
# runs this wrapper) from taking the store lock or renewing an OAuth token over the network.
if [ "$#" -eq 1 ] && [ "$1" = version ]; then
  exec "$real" "$@"
fi

if [ -n "${JIRA_API_TOKEN:-}" ] || [ -n "${JIRA_SERVER:-}" ]; then
  exec "$real" "$@"
fi

# The config jira will read, the subcommand, and whether this is a debug run, from argv. Cobra
# takes the last -c, in any of -c X, -cX, -c=X, --config X, --config=X.
cfg="" sub="" own_init="" debug="" prev=""
for a in "$@"; do
  case "$prev" in
    -c | --config) cfg="$a"; prev=""; continue ;;
    -p | --project) prev=""; continue ;;
  esac
  case "$a" in
    --) break ;;
    -c | --config | -p | --project) prev="$a" ;;
    --config=*) cfg="${a#--config=}" ;;
    -c=*) cfg="${a#-c=}" ;;
    -c?*) cfg="${a#-c}" ;;
    --debug | --debug=[tT1]* ) debug=1 ;;
    --server | --server=* | --login | --login=* | --installation | --installation=* | --auth-type | --auth-type=*)
      own_init=1 ;;
    -*) ;;
    *) [ -z "$sub" ] && sub="$a" ;;
  esac
done
case "$sub" in initialize | configure | config | setup) sub=init ;; esac
case "${JIRA_DEBUG:-}" in "" | 0 | f* | F*) ;; *) debug=1 ;; esac
[ -n "$cfg" ] || cfg="${JIRA_CONFIG_FILE:-}"

conf="${XDG_CONFIG_HOME:-${HOME:-/nonexistent}/.config}"
managed="$conf/agent-fleet/jira"
in_managed() {
  local p m
  p="$(realpath -m -- "$1" 2>/dev/null)" && m="$(realpath -m -- "$managed" 2>/dev/null)" &&
    [ -n "$p" ] && [ -n "$m" ] && case "$p" in "$m"/*) true ;; *) false ;; esac
}

if [ "$sub" = init ]; then
  # `jira init` naming its own server or login is configuring another Jira.
  [ -z "$own_init" ] || exec "$real" "$@"
  if [ -z "$cfg" ]; then
    export JIRA_CONFIG_FILE="$managed/config.yml"
  elif ! in_managed "$cfg"; then
    exec "$real" "$@"
  fi
elif [ -n "$cfg" ]; then
  in_managed "$cfg" || exec "$real" "$@"
elif [ -e "$conf/.jira/.config.yml" ] || [ -e "$conf/.jira/.config.yaml" ]; then
  exec "$real" "$@"
elif [ -e "$managed/config.yml" ]; then
  export JIRA_CONFIG_FILE="$managed/config.yml"
else
  gen="${XDG_CACHE_HOME:-${HOME:-/nonexistent}/.cache}/agent-fleet/jira/config.yml"
  if [ ! -s "$gen" ]; then
    mkdir -p "${gen%/*}" 2>/dev/null &&
      printf '# Written by the jira wrapper (no secrets). Run `jira init --project KEY --board NAME`\n# for a default project, board, issue types and epic fields.\ninstallation: Cloud\n' >"$gen" 2>/dev/null
  fi
  [ -s "$gen" ] && export JIRA_CONFIG_FILE="$gen"
fi
if [ -n "$debug" ]; then
  echo "jira: debug output prints the Authorization header, so this run gets no credential from the connection" >&2
  exec "$real" "$@"
fi

# First value of a `key=value` line in the helper's answer on stdin.
field() { sed -n "s/^$1=//;T;p;q"; }

cred="$(workspace-agent jira-cred 2>/dev/null)" || cred=""
tok="$(printf '%s\n' "$cred" | field token)"
server="$(printf '%s\n' "$cred" | field server)"
auth="$(printf '%s\n' "$cred" | field auth)"
login="$(printf '%s\n' "$cred" | field login)"
if [ -z "$tok" ] || [ -z "$server" ] || [ -z "$auth" ]; then
  exec "$real" "$@"
fi

if [ "$sub" = init ]; then
  # A bearer login is replaced by what /myself reports, so the placeholder only gets past
  # init's prompt for an empty one.
  set -- "$@" --installation cloud --server "$server" --auth-type "$auth" --login "${login:-oauth}"
fi

export JIRA_API_TOKEN="$tok" JIRA_SERVER="$server" JIRA_AUTH_TYPE="$auth" JIRA_INSTALLATION=Cloud JIRA_DEBUG=false
if [ -n "$login" ]; then export JIRA_LOGIN="$login"; else unset JIRA_LOGIN; fi
exec "$real" "$@"
