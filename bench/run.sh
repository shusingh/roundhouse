#!/usr/bin/env bash
# Runs the benchmarks in docs/engineering/design.md section 15 against a
# real roundhoused on this host and collects the raw results, one JSON line
# per measurement, in bench/results/<date>/.
#
# Usage: sudo bash bench/run.sh [RUNS]
#   RUNS  repetitions of each configuration (default 3)
#
# Needs: make build (bin/roundhoused, bin/rhbench), the guest image, and
# bench/setup-baselines.sh for the container baselines. Sequential start runs
# use N creates (default 1000; override with N=...). SCENARIOS selects what to
# run (default "cold restore pool baselines").
set -uo pipefail

runs=${1:-3}
n=${N:-1000}
scenarios=" ${SCENARIOS:-cold restore pool baselines} "
want() { [[ $scenarios == *" $1 "* ]]; }
repo=$(cd "$(dirname "$0")/.." && pwd)
out="$repo/bench/results/$(date -u +%Y-%m-%d)"
state=/var/lib/roundhouse/bench
mkdir -p "$out" "$state"
log() { echo "[$(date -u +%H:%M:%S)] $*" >&2; }

keyline=$("$repo/bin/roundhoused" hash-key)
ROUNDHOUSE_API_KEY=$(awk '/^key:/ {print $2}' <<<"$keyline")
export ROUNDHOUSE_API_KEY
digest=$(awk '/^sha256:/ {print $2}' <<<"$keyline")
export ROUNDHOUSE_URL=http://127.0.0.1:8490

# write_config POOL_YAML: a config for one scenario. The budget is raised so
# the host's memory, not admission, bounds the density run.
write_config() {
  cat >"$state/roundhoused.yaml" <<EOF
server:
  listen: 127.0.0.1:8490
log_level: warn
host:
  memory_budget_mib: 10000000
  vcpu_budget: 100000
sandbox:
  max_sandboxes: 1000
  max:
    memory_mib: 4096
$1
api_keys:
  - name: bench
    sha256: $digest
EOF
}

server_pid=
start_server() {
  "$repo/bin/roundhoused" -config "$state/roundhoused.yaml" >>"$out/roundhoused.log" 2>&1 &
  server_pid=$!
  for _ in $(seq 1 100); do
    if curl -fs "$ROUNDHOUSE_URL/readyz" >/dev/null 2>&1; then return 0; fi
    sleep 0.1
  done
  log "roundhoused did not become ready"; exit 1
}
stop_server() {
  if [[ -n $server_pid ]]; then
    kill -TERM "$server_pid" 2>/dev/null
    wait "$server_pid" 2>/dev/null
    server_pid=
  fi
}
trap stop_server EXIT

bench() { # NAME ARGS...: run rhbench, appending its result to NAME.jsonl
  local name=$1; shift
  log "$name: rhbench $*"
  "$repo/bin/rhbench" "$@" >>"$out/$name.jsonl" || log "$name failed"
}

tpl_dir=/var/lib/roundhouse/images/default/templates/1cpu-256mib

# 1. Cold boot: no template for the shape.
rm -rf "$tpl_dir"
write_config ""
if want cold; then
  start_server
  for _ in $(seq 1 "$runs"); do
    bench start-cold start -n "$n" -c 1 -memory-mib 256
    bench start-cold-c16 start -n "$n" -c 16 -memory-mib 256
  done
  stop_server
fi

# 2. Restore: a template, no pool.
"$repo/bin/roundhoused" template build -config "$state/roundhoused.yaml" -memory-mib 256 >>"$out/template.log" 2>&1
if want restore; then
  start_server
  for _ in $(seq 1 "$runs"); do
    bench start-restore start -n "$n" -c 1 -memory-mib 256
    bench start-restore-c16 start -n "$n" -c 16 -memory-mib 256
    bench exec exec -n 10000
    bench neighbor neighbor -seconds 10
    bench density density -memory-mib 256 -reserve-mib 4096
  done
  stop_server
fi

# 3. Warm pool: claims measured one at a time with the pool refilled, then
# 16-way, where claims outpace the refill and some creates restore instead.
write_config "  pools:
    - memory_mib: 256
      size: 16"
if want pool; then
  start_server
  for _ in $(seq 1 "$runs"); do
    bench start-pool start -n "$n" -c 1 -memory-mib 256 -wait-pool
    bench start-pool-c16 start -n "$n" -c 16 -memory-mib 256
  done
  stop_server
fi
rm -rf "$tpl_dir"

# 4. Container baselines on the same root file system.
want baselines && for _ in $(seq 1 "$runs"); do
  bench baseline-runc baseline -runtime /usr/local/bin/runc -bundle "$state/bundle" -n "$n" -exec-n 1000
  bench baseline-runsc baseline -runtime /usr/local/bin/runsc -runtime-args "--network=none" -bundle "$state/bundle" -n "$n" -exec-n 1000
done

log "results in $out"
