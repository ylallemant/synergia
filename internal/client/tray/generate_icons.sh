#!/usr/bin/env bash
INKSCAPE="${INKSCAPE:-/Applications/Inkscape.app/Contents/MacOS/inkscape}"
# Generate tray icon PNGs from phi.svg using Inkscape.
# Run from the repo root or from this directory.
# Requires: Inkscape 1.x (inkscape --version to confirm)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SVG="$SCRIPT_DIR/phi.svg"
SIZE=32   # px — systray scales to fit; 32 is a good base

generate() {
  local NAME="$1" COLOR="$2"
  local TMP
  TMP="$(mktemp /tmp/synergia_icon_XXXXXX.svg)"
  sed "s/#c84529/$COLOR/g" "$SVG" > "$TMP"
  "$INKSCAPE" \
    --export-type=png \
    --export-filename="$SCRIPT_DIR/${NAME}.png" \
    --export-width=$SIZE \
    --export-height=$SIZE \
    "$TMP" 2>/dev/null
  rm "$TMP"
  echo "generated ${NAME}.png"
}

generate phi_connected    "#4caf50"
generate phi_processing   "#2196f3"
generate phi_reconnecting "#ffc107"
generate phi_paused       "#9e9e9e"
generate phi_disconnected "#f44336"

echo "done — all icons written to $SCRIPT_DIR"
