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
# - `issue create` needs issue.types, which only `jira init` writes (and it needs the Jira Software
#   scopes). For a config of ours that has none, the types are fetched by the Agent
#   (`workspace-agent jira-issue-types`, createmeta, classic scope) into a derived copy of the
#   config under the cache directory; that never replaces a config the user owns.
# - Fails closed: any helper error leaves jira running with no injected auth.
# - The token only ever lives in this shell's variables and, at the very end, in the exec'd
#   jira's environment: never argv, never a file, never printed. xtrace and allexport are
#   switched off first, and the variables are unset so none inherits an export attribute:
#   otherwise the assignments below would be echoed, or exported to every helper.
set +xa
unset cred tok server auth login cfg sub sub2 proj cfg_argv own_init debug prev conf managed gen
unset eff key cdir types derived yaml hint

real="${AF_JIRA_REAL:-/usr/local/libexec/jira}"

# `jira version` needs no credential. Skipping the helper keeps the Toolchains probe (which
# runs this wrapper) from taking the store lock or renewing an OAuth token over the network.
if [ "$#" -eq 1 ] && [ "$1" = version ]; then
  exec "$real" "$@"
fi

if [ -n "${JIRA_API_TOKEN:-}" ] || [ -n "${JIRA_SERVER:-}" ]; then
  exec "$real" "$@"
fi

# The config jira will read, the subcommand (and the word after it), the project, and whether
# this is a debug run, from argv. Cobra takes the last -c, in any of -c X, -cX, -c=X, --config X,
# --config=X; -p / --project the same way.
cfg="" sub="" sub2="" proj="" own_init="" debug="" prev=""
for a in "$@"; do
  case "$prev" in
    -c | --config) cfg="$a"; prev=""; continue ;;
    -p | --project) proj="$a"; prev=""; continue ;;
  esac
  case "$a" in
    --) break ;;
    -c | --config | -p | --project) prev="$a" ;;
    --config=*) cfg="${a#--config=}" ;;
    -c=*) cfg="${a#-c=}" ;;
    -c?*) cfg="${a#-c}" ;;
    --project=*) proj="${a#--project=}" ;;
    -p=*) proj="${a#-p=}" ;;
    -p?*) proj="${a#-p}" ;;
    --debug | --debug=[tT1]* ) debug=1 ;;
    --server | --server=* | --login | --login=* | --installation | --installation=* | --auth-type | --auth-type=*)
      own_init=1 ;;
    -*) ;;
    *) if [ -z "$sub" ]; then sub="$a"; elif [ -z "$sub2" ]; then sub2="$a"; fi ;;
  esac
done
case "$sub" in initialize | configure | config | setup) sub=init ;; esac
case "${JIRA_DEBUG:-}" in "" | 0 | f* | F*) ;; *) debug=1 ;; esac
cfg_argv="$cfg"
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

# `jira issue create` reads the issue types from the config only, and `jira init` (the only
# writer) needs the Jira Software scopes. When the effective config is ours and has none, fetch
# them through createmeta (classic scope, `workspace-agent jira-issue-types`, which holds the
# token itself) into a derived, secret-free config next to the generated one. Any failure leaves
# jira running on the config it had, after a hint that names the next step.
has_types() { grep -Eiq '^[[:space:]]*"?types"?[[:space:]]*:' "$1" 2>/dev/null; }
types_hint() {
  echo "jira: this config has no issue types, which \`issue create\` needs; $1 (find the exact board name with \`jira board list -p KEY\`; a 401 from init can mean the Jira app lacks the Jira Software scopes, which needs a Jira reconnect)" >&2
}
# True when appending an `issue:` block to the file cannot duplicate or reshape a key: the first
# real line starts at column 0 and every column-0 line is a bare `key:` (no quotes, no flow
# style, no `---` / `...` document marks), none of them `issue`. Anything else is not merged.
appendable() {
  local first
  first="$(sed -n '/^[[:space:]]*\(#.*\)\{0,1\}$/d;p;q' "$1" 2>/dev/null)"
  case "$first" in "" | [[:space:]]*) return 1 ;; esac
  grep -Ev '^([[:space:]]|#|$)' "$1" 2>/dev/null | grep -Evq '^[A-Za-z_][A-Za-z0-9_-]*:([[:space:]]|$)' && return 1
  grep -Eiq '^issue[[:space:]]*:' "$1" 2>/dev/null && return 1
  return 0
}
# Writes the derived config to a private directory and points JIRA_CONFIG_FILE at it. The base is
# copied whole, so the files are 0600 in a 0700 directory whatever the caller's umask, and the
# name carries a hash of the content: a concurrent run with another base (or another site's
# types) gets its own file instead of rewriting one jira has yet to read. The types cache is kept
# per connection (a hash of the server URL, which holds no secret) and project.
derive_types_config() {
  local base="$1" ddir types derived yaml sh ch snap
  appendable "$base" || return 1
  key="${proj:-}"
  [ -n "$key" ] || key="$(sed -n '/^project:/,/^[^[:space:]#]/{s/^[[:space:]][[:space:]]*key:[[:space:]]*["'"'"']*\([A-Za-z0-9_]*\).*/\1/p;T;q}' "$base" 2>/dev/null)"
  [ -n "$key" ] || return 2
  case "$key" in [A-Za-z]*) ;; *) return 1 ;; esac
  case "$key" in *[!A-Za-z0-9_]*) return 1 ;; esac
  key="${key^^}"
  sh="$(printf '%s' "$server" | sha256sum 2>/dev/null | cut -c1-12)"
  [ -n "$sh" ] || return 1
  ddir="${XDG_CACHE_HOME:-${HOME:-/nonexistent}/.cache}/agent-fleet/jira/derived"
  types="$ddir/issue-types-$sh-$key.yml"
  (umask 077; mkdir -p "$ddir" 2>/dev/null) && chmod 700 "$ddir" 2>/dev/null || return 1
  yaml="$(workspace-agent jira-issue-types "$key" 2>/dev/null)" || yaml=""
  case "$yaml" in
    "issue:"*"- id:"*) (umask 077; printf '%s\n' "$yaml" >"$types.$$") 2>/dev/null && mv -f "$types.$$" "$types" 2>/dev/null ;;
  esac
  rm -f "$types.$$" 2>/dev/null
  [ -s "$types" ] || return 1
  # Read base and types once into a private temp: the hash names exactly what is published, even
  # when another run swaps the types cache in between.
  snap="$ddir/snap.$$"
  (umask 077; { cat "$base" && printf '\n' && cat "$types"; } >"$snap") 2>/dev/null || { rm -f "$snap"; return 1; }
  ch="$(sha256sum <"$snap" 2>/dev/null | cut -c1-16)"
  [ -n "$ch" ] || { rm -f "$snap"; return 1; }
  derived="$ddir/config-$key-$ch.yml"
  if [ -s "$derived" ]; then
    rm -f "$snap"
    touch -c "$derived" 2>/dev/null
  else
    mv -f "$snap" "$derived" 2>/dev/null || { rm -f "$snap"; return 1; }
  fi
  # Never the file this run is about to hand to jira; the age limit protects other runs' files.
  find "$ddir" -name 'config-*.yml' ! -name "${derived##*/}" -mtime +1 -delete 2>/dev/null
  export JIRA_CONFIG_FILE="$derived"
}
if { [ "$sub" = issue ] || [ "$sub" = issues ]; } && [ "$sub2" = create ]; then
  eff="${cfg_argv:-${JIRA_CONFIG_FILE:-}}"
  if [ -n "$eff" ] && [ -r "$eff" ] && ! has_types "$eff"; then
    if [ -n "$cfg_argv" ]; then
      types_hint "run \`jira init --project KEY --board NAME\`"
    else
      derive_types_config "$eff"
      case $? in
        0) ;;
        2) types_hint "pass \`-p KEY\` or run \`jira init --project KEY --board NAME\`" ;;
        *) types_hint "run \`jira init --project KEY --board NAME\`" ;;
      esac
    fi
  fi
fi

if [ "$sub" = init ]; then
  # A bearer login is replaced by what /myself reports, so the placeholder only gets past
  # init's prompt for an empty one.
  set -- "$@" --installation cloud --server "$server" --auth-type "$auth" --login "${login:-oauth}"
fi

export JIRA_API_TOKEN="$tok" JIRA_SERVER="$server" JIRA_AUTH_TYPE="$auth" JIRA_INSTALLATION=Cloud JIRA_DEBUG=false
if [ -n "$login" ]; then export JIRA_LOGIN="$login"; else unset JIRA_LOGIN; fi
exec "$real" "$@"
