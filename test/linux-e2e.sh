#!/usr/bin/env bash
# Linux end to end on a CPU-only host: an RPC node and a llama-server split across it, the server run
# under systemd so its log goes to the journal, then a llamesh server and collector, and a request.
# Checks what the page would receive. Writes what it saw to $OUT for recording as fixtures.
set -euo pipefail
LLAMA=${LLAMA:?directory holding llama-server and ggml-rpc-server}
MODEL=${MODEL:?path to a GGUF}
OUT=${OUT:-/tmp/llamesh-e2e}
mkdir -p "$OUT"
export LLAMESH_TOKEN=e2e-token

go build -o "$OUT/llamesh" ./cmd/llamesh
command -v lsof >/dev/null || sudo apt-get install -y lsof

"$LLAMA/ggml-rpc-server" -H 127.0.0.1 -p 50052 > "$OUT/rpc-server.log" 2>&1 &
sleep 2
sudo systemd-run --unit=llamesh-e2e --uid="$(id -u)" --setenv=LD_LIBRARY_PATH="$LLAMA" \
  "$LLAMA/llama-server" -m "$MODEL" --host 127.0.0.1 --port 8896 -c 2048 \
  --rpc 127.0.0.1:50052 --no-mmap -lv 4
for _ in $(seq 60); do curl -sf http://127.0.0.1:8896/health >/dev/null && break; sleep 1; done
curl -sf http://127.0.0.1:8896/health

"$OUT/llamesh" -server -listen 127.0.0.1:8899 -ingest 127.0.0.1:8900 > "$OUT/server.log" 2>&1 &
# as root: the server's journal and /proc belong to other users
sudo -E "$OUT/llamesh" -collector 127.0.0.1:8900 > "$OUT/collector.log" 2>&1 &
sleep 12
( for _ in 1 2 3; do curl -s http://127.0.0.1:8896/completion -d '{"prompt":"Describe an orbit.","n_predict":96}' >/dev/null; done ) &
requests=$!
timeout 30 curl -sN http://127.0.0.1:8899/api/stream > "$OUT/stream.txt" || true
wait "$requests" || true

pid=$(systemctl show -p MainPID --value llamesh-e2e)
sudo journalctl _PID="$pid" -o cat --no-pager > "$OUT/llama-server-journal.log"
cp /proc/net/dev "$OUT/proc-net-dev.txt"
ip route get 127.0.0.1 > "$OUT/ip-route-get.txt"
sudo systemctl stop llamesh-e2e || true

frames=$(sed -n 's/^data: //p' "$OUT/stream.txt")
fail() { echo "FAIL: $*"; echo "--- collector"; cat "$OUT/collector.log"; exit 1; }
[ -n "$frames" ] || fail "no frames on the page's stream"
grep -q "log the systemd journal" "$OUT/collector.log" || fail "the collector did not read the journal"
last=$(echo "$frames" | tail -1)
echo "$last" | jq -e '.nodes[] | select(.kind=="KIND_LLAMA_SERVER") | .device=="CPU" and .mem_model>0 and .mem_total>0 and .layers!=null' >/dev/null || fail "local CPU device: $(echo "$last" | jq -c '.nodes[0]')"
echo "$last" | jq -e '.nodes[] | select(.kind=="KIND_RPC") | .mem_model>0 and .layers!=null' >/dev/null || fail "rpc node: $(echo "$last" | jq -c '.nodes[1]')"
echo "$last" | jq -e '.links[0].iface=="lo"' >/dev/null || fail "link interface: $(echo "$last" | jq -c '.links')"
echo "$frames" | jq -e -s 'any(.[]; .links[0].bytes_out_per_s > 0)' >/dev/null || fail "no link traffic during the requests"
echo "$frames" | jq -e -s 'any(.[]; .nodes[0].tokens_per_s > 0)' >/dev/null || fail "no token rate during the requests"
echo "PASS: $(echo "$frames" | wc -l) frames; $(echo "$last" | jq -c '[.nodes[] | {kind, device, layers, mem_model}]')"
