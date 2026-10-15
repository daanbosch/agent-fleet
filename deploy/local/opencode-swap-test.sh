#!/usr/bin/env bash
# The entrypoint's oc_install_swap moves the other opencode package aside for an `npm install -g`
# and puts it back when the install fails. Runs the real function against a stub `npm` that
# behaves like the real one where it matters: EEXIST while the other package owns the `opencode`
# bin, success otherwise, and a switch to fail the install (a registry error or another CLI).
#
#   deploy/local/opencode-swap-test.sh
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail() { echo "NG: $1"; exit 1; }

mkdir -p "$WORK/bin"
cat > "$WORK/bin/npm" <<'FAKE'
#!/usr/bin/env bash
# npm install -g --prefix <p> <spec>...
[ "$1" = install ] || exit 0
pfx="$4"; shift 4
[ "${STUB_NPM_FAIL:-}" != 1 ] || exit 1
for spec in "$@"; do
  pkg="${spec%@*}"
  case "$pkg" in opencode-ai|@opencode/cli)
    [ ! -e "$pfx/bin/opencode" ] || { echo "npm error EEXIST" >&2; exit 1; }
    mkdir -p "$pfx/lib/node_modules/$pkg/bin"
    echo "$pkg" > "$pfx/lib/node_modules/$pkg/marker"
    ln -s "../lib/node_modules/$pkg/bin/opencode.exe" "$pfx/bin/opencode" ;;
  esac
done
FAKE
chmod +x "$WORK/bin/npm"
export PATH="$WORK/bin:$PATH"

# The function, cut out of the entrypoint (a file that no longer has it fails the extraction).
sed -n '/^oc_install_swap() {$/,/^}$/p' "$ROOT/workspace/entrypoint.sh" > "$WORK/fn.sh"
[ -s "$WORK/fn.sh" ] || fail "workspace/entrypoint.sh: oc_install_swap() was not found"

home_with_v1() { # a prefix whose opencode is opencode-ai
  rm -rf "$WORK/p"; mkdir -p "$WORK/p/bin"
  STUB_NPM_FAIL=0 npm install -g --prefix "$WORK/p" opencode-ai@1.19.0
}
swap() { # swap <fail 0|1>
  (OC_OTHER=opencode-ai; STUB_NPM_FAIL="$1"; export STUB_NPM_FAIL; . "$WORK/fn.sh"
   oc_install_swap "$WORK/p" @anthropic-ai/claude-code@latest @opencode/cli@latest)
}
owner() { cat "$WORK/p/lib/node_modules/$(readlink "$WORK/p/bin/opencode" | sed 's|.*node_modules/\(.*\)/bin/.*|\1|')/marker"; }

# Control: without the swap the stub behaves like npm and refuses (EEXIST), so a pass below is
# the function's doing and not the stub's.
home_with_v1
if STUB_NPM_FAIL=0 npm install -g --prefix "$WORK/p" @opencode/cli@latest 2>/dev/null; then fail "control: the stub did not refuse the install over the other package"; fi

home_with_v1
swap 0 || fail "the swap failed although npm succeeds"
[ "$(owner)" = "@opencode/cli" ] || fail "after a successful swap the bin belongs to '$(owner)'"
[ ! -e "$WORK/p/lib/node_modules/opencode-ai" ] || fail "the other package was left installed after a successful swap"
[ ! -e "$WORK/p/.af-oc-swap" ] || fail "the backup directory was left behind"
echo "ok  success: opencode now belongs to @opencode/cli"

home_with_v1
if swap 1; then fail "the swap reported success although npm failed"; fi
[ "$(owner)" = "opencode-ai" ] || fail "after a failed install opencode belongs to '$(owner)', want the original opencode-ai"
[ -x "$WORK/p/bin/opencode" ] || [ -L "$WORK/p/bin/opencode" ] || fail "the opencode bin is gone"
[ ! -e "$WORK/p/.af-oc-swap" ] || fail "the backup directory was left behind"
echo "ok  failure: the original opencode-ai and its bin are back"

# Nothing to move aside: the install is just the install.
rm -rf "$WORK/p"; mkdir -p "$WORK/p/bin"
swap 0 || fail "the swap failed on a prefix with no other package"
[ "$(owner)" = "@opencode/cli" ] || fail "a clean prefix did not get @opencode/cli"
echo "ok  clean prefix"
echo "OK: oc_install_swap swaps the opencode package and restores it on failure"
