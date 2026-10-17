#!/usr/bin/env bash
# Bitbucket CLI (bkt) transparent-auth wrapper — the Bitbucket counterpart of gh-auth-wrapper.sh.
#
# bkt reads neither git's credential helper nor the Agent's store, and the container has no OS
# keychain for `bkt auth login`. This sits in front of the real binary (/usr/local/libexec/bkt),
# asks the Agent for the Connections credential on every call (so an expired OAuth token has
# already been refreshed, exactly as for git) and hands it to bkt through its headless
# environment variables (docs/build/08 §8.3).
#
# - An explicit BKT_TOKEN / BKT_HOST, or a config file bkt itself wrote (`bkt auth login`,
#   `bkt context create`, e.g. a Data Center host), is the user's own setup: exec unchanged.
#   Injecting BKT_HOST here would override their contexts.
# - Fails closed: any helper error leaves bkt running with no injected auth.
# - The token only ever lives in this shell's variables and, at the very end, in the exec'd
#   bkt's environment: never argv, never a file, never printed. xtrace is switched off first
#   because an inherited SHELLOPTS=xtrace would echo every assignment below.
set +x

real="${AF_BKT_REAL:-/usr/local/libexec/bkt}"

# `bkt --version` needs no credential. Skipping the helper keeps the Toolchains probe (which
# runs this wrapper) from taking the store lock or refreshing an OAuth token over the network,
# which with a connection can outlast the probe timeout and report the effective column as a
# timeout.
if [ "$#" -eq 1 ] && [ "$1" = "--version" ]; then
  exec "$real" "$@"
fi

if [ -n "${BKT_TOKEN:-}" ] || [ -n "${BKT_HOST:-}" ] ||
  [ -e "${BKT_CONFIG_DIR:-${XDG_CONFIG_HOME:-${HOME:-/nonexistent}/.config}/bkt}/config.yml" ]; then
  exec "$real" "$@"
fi

# First value of a `key=value` line in the credential-protocol text on stdin.
field() { sed -n "s/^$1=//;T;p;q"; }

cred="$(printf 'protocol=https\nhost=bitbucket.org\n\n' | GIT_TERMINAL_PROMPT=0 git credential fill 2>/dev/null)" || cred=""
user="$(printf '%s\n' "$cred" | field username)"
tok="$(printf '%s\n' "$cred" | field password)"
method=""

case "$user" in
  # OAuth connection: the helper's token is a Bitbucket access token, sent as a bearer.
  x-token-auth) [ -n "$tok" ] && method=bearer ;;
  # Pasted API token: git gets a static username, the REST API wants the Atlassian email.
  x-bitbucket-api-token-auth)
    cred="$(workspace-agent bkt-cred 2>/dev/null)" || cred=""
    user="$(printf '%s\n' "$cred" | field username)"
    tok="$(printf '%s\n' "$cred" | field password)"
    [ -n "$user" ] && [ -n "$tok" ] && method=basic
    ;;
esac

if [ -n "$method" ]; then
  export BKT_TOKEN="$tok" BKT_HOST=https://bitbucket.org BKT_AUTH_METHOD="$method"
  if [ "$method" = basic ]; then export BKT_USERNAME="$user"; else unset BKT_USERNAME; fi
fi
exec "$real" "$@"
