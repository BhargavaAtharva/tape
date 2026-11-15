#!/bin/zsh
# Measures what one browser tab costs: runs the server (in memory, bots on),
# the counting proxy and the dev server, opens the app in headless Chrome
# and reports bytes after <seconds>. Needs bin/tape built and Google Chrome.
# usage: web/scripts/measure-tab.sh <repo-dir> <seconds>
set -e
DIR=$1; SECS=$2
cd "$DIR"
TAPE_DATA_DIR= PORT=8081 ./bin/tape > /tmp/mt-server.log 2>&1 &
SRV=$!
sleep 3
node "$DIR/web/scripts/count-proxy.mjs" 8090 127.0.0.1:8081 $((SECS+5)) > /tmp/mt-proxy.txt 2>&1 &
PRX=$!
(cd web && TAPE_DEV_API=http://127.0.0.1:8090 npx vite --port 5174 --strictPort > /tmp/mt-vite.log 2>&1 &)
sleep 3
"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" --headless=new --disable-gpu --user-data-dir=$(mktemp -d) --remote-debugging-port=9333 --window-size=1440,900 "http://localhost:5174/#BTC-USD" > /dev/null 2>&1 &
CHR=$!
wait $PRX
kill $CHR; pkill -f "vite --port 5174"; kill $SRV
cat /tmp/mt-proxy.txt
