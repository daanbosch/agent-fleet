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
sed -n '/^oc_swap_restore() {/,/^}$/p;/^oc_install_swap() {$/,/^}$/p' "$ROOT/workspace/entrypoint.sh" > "$WORK/fn.sh"
[ -s "$WORK/fn.sh" ] || fail "workspace/entrypoint.sh: oc_install_swap() was not found"

home_with() { # home_with <pkg>: a prefix whose opencode is <pkg>
  rm -rf "$WORK/p"; mkdir -p "$WORK/p/bin"
  STUB_NPM_FAIL=0 npm install -g --prefix "$WORK/p" "$1@1.0.0"
}
home_with_v1() { home_with opencode-ai; }
swap() { # swap <fail 0|1> [<other> <new>]: the install carries a second CLI, as boot-install and self-update do
  (OC_OTHER="${2:-opencode-ai}"; STUB_NPM_FAIL="$1"; export STUB_NPM_FAIL; . "$WORK/fn.sh"
   oc_install_swap "$WORK/p" @anthropic-ai/claude-code@latest "${3:-@opencode/cli}@latest")
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

# Reverse direction (a rollback from 2.x to 1.x), success and failure.
home_with @opencode/cli
swap 0 @opencode/cli opencode-ai || fail "the reverse swap failed although npm succeeds"
[ "$(owner)" = "opencode-ai" ] || fail "reverse: the bin belongs to '$(owner)'"
home_with @opencode/cli
if swap 1 @opencode/cli opencode-ai; then fail "reverse: reported success although npm failed"; fi
[ "$(owner)" = "@opencode/cli" ] || fail "reverse: after a failed install the bin belongs to '$(owner)'"
echo "ok  reverse direction: swap, and restore on failure"

# A start that stopped right after moving the copy aside: the aside copy is the only one. The
# next start must put it back before it installs, also when that install then fails.
home_with_v1
mkdir -p "$WORK/p/.af-oc-swap/pkg" "$WORK/p/.af-oc-swap/bin"
mv "$WORK/p/lib/node_modules/opencode-ai" "$WORK/p/.af-oc-swap/pkg/opencode-ai"
mv "$WORK/p/bin/opencode" "$WORK/p/.af-oc-swap/bin/opencode"
if swap 1; then fail "resume: reported success although npm failed"; fi
[ "$(owner)" = "opencode-ai" ] || fail "resume after an interrupted swap, install failing: opencode is '$(owner)', want the original back"
home_with_v1
mkdir -p "$WORK/p/.af-oc-swap/pkg" "$WORK/p/.af-oc-swap/bin"
mv "$WORK/p/lib/node_modules/opencode-ai" "$WORK/p/.af-oc-swap/pkg/opencode-ai"
mv "$WORK/p/bin/opencode" "$WORK/p/.af-oc-swap/bin/opencode"
swap 0 || fail "resume: the swap failed although npm succeeds"
[ "$(owner)" = "@opencode/cli" ] || fail "resume after an interrupted swap, install ok: bin belongs to '$(owner)'"
echo "ok  interrupted swap: the aside copy is restored first, never deleted"

# Stopped again while restoring: the package is back, the bin is still aside. A failing install
# must still leave a runnable opencode (the bin is restored independently of the package).
home_with_v1
mkdir -p "$WORK/p/.af-oc-swap/bin"
mv "$WORK/p/bin/opencode" "$WORK/p/.af-oc-swap/bin/opencode"
if swap 1; then fail "half-restored: reported success although npm failed"; fi
[ "$(owner)" = "opencode-ai" ] || fail "half-restored (pkg back, bin aside), install failing: opencode is '$(owner)'"
echo "ok  half-restored swap: the aside bin comes back"

# Moved aside by the 1.x -> 2.x update, then the image was rolled back to 1.x: OC_OTHER is now
# @opencode/cli, and the aside copy is opencode-ai. It must be restored by its own name.
home_with_v1
mkdir -p "$WORK/p/.af-oc-swap/pkg" "$WORK/p/.af-oc-swap/bin"
mv "$WORK/p/lib/node_modules/opencode-ai" "$WORK/p/.af-oc-swap/pkg/opencode-ai"
mv "$WORK/p/bin/opencode" "$WORK/p/.af-oc-swap/bin/opencode"
if swap 1 @opencode/cli opencode-ai; then fail "re-pinned: reported success although npm failed"; fi
[ "$(owner)" = "opencode-ai" ] || fail "aside opencode-ai after a re-pin to 1.x, install failing: opencode is '$(owner)'"
[ ! -e "$WORK/p/.af-oc-swap" ] || fail "the backup directory was left behind after a complete restore"
echo "ok  re-pinned during a swap: restored by the aside copy's own name"

# The boot-install and self-update paths both go through the helper and nothing uninstalls first.
[ "$(grep -c 'oc_install_swap "' "$ROOT/workspace/entrypoint.sh")" -ge 2 ] || fail "entrypoint.sh: boot-install and self-update must both call oc_install_swap"
! grep -q 'npm uninstall -g --prefix "$HOME/.local" \$NPM_DROP\|NPM_DROP' "$ROOT/workspace/entrypoint.sh" || fail "entrypoint.sh: a drop-then-install path is left"
echo "ok  both install paths use the helper"

# Nothing to move aside: the install is just the install.
rm -rf "$WORK/p"; mkdir -p "$WORK/p/bin"
swap 0 || fail "the swap failed on a prefix with no other package"
[ "$(owner)" = "@opencode/cli" ] || fail "a clean prefix did not get @opencode/cli"
echo "ok  clean prefix"
echo "OK: oc_install_swap swaps the opencode package and restores it on failure"
