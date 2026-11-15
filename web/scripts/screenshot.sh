#!/bin/zsh
# Captures docs/screenshot.png: runs the server in memory with the bots on,
# lets the market build up, serves the app with Vite and renders it in
# headless Chrome. Needs bin/tape built and Google Chrome.
# usage: web/scripts/screenshot.sh [warmup-seconds]
set -e
ROOT=${0:A:h:h:h}
WARMUP=${1:-30}
cd "$ROOT"
TAPE_DATA_DIR= PORT=8081 ./bin/tape > /tmp/shot-server.log 2>&1 &
SRV=$!
(cd web && npx vite --port 5174 --strictPort > /tmp/shot-vite.log 2>&1 &)
trap 'kill $SRV; pkill -f "vite --port 5174"' EXIT
sleep $WARMUP
"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" --headless=new --disable-gpu --hide-scrollbars \
  --user-data-dir="$(mktemp -d)" --window-size=1440,900 --virtual-time-budget=8000 \
  --screenshot="$ROOT/docs/screenshot.png" "http://localhost:5174/#BTC-USD" > /dev/null 2>&1
echo "wrote docs/screenshot.png"
