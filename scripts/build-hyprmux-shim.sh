#!/bin/sh
# Builds the hyprmux shim that the hyprmux-host runtime execs (AF_HYPRMUX_SHIM).
# The shim is its own Go module, so it is built from its own directory.
#   usage: scripts/build-hyprmux-shim.sh <output-path>
set -eu
out=${1:?usage: build-hyprmux-shim.sh <output-path>}
root=$(cd "$(dirname "$0")/.." && pwd)
(cd "$root/workspace/hyprmux-shim" && go build -o "$out" .)
echo "$out"
