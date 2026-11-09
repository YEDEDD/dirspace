#!/usr/bin/env bash
# 只读性能验证脚本。不会创建或修改业务文件。
# 默认只测查询延迟；扫描测试需要显式 PERF_ALLOW_SCAN=1。
set -euo pipefail

BIN="${DIRSPACE_BIN:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/bin/dirspace}"
ROOT="${PERF_ROOT:-/mnt}"
DB="${PERF_DB:-/var/lib/dirspace/usage.db}"
CONFIG="${PERF_CONFIG:-/etc/dirspace/config.yaml}"
RUNS="${PERF_RUNS:-50}"

if [[ ! -x "$BIN" ]]; then
  echo "未找到可执行文件: $BIN（先 make build-linux）" >&2
  exit 1
fi

echo "== 查询延迟（只读 SQLite，不访问 NFS） =="
echo "binary=$BIN root=$ROOT db=$DB runs=$RUNS"
if [[ ! -f "$DB" ]]; then
  echo "数据库不存在，跳过。请先建立基线。" >&2
  exit 0
fi

config_arg=()
if [[ -f "$CONFIG" ]]; then
  config_arg=(--config "$CONFIG")
fi
tmp="$(mktemp)"
trap 'rm -f "$tmp" "${tmp}.sorted"' EXIT
for i in $(seq 1 "$RUNS"); do
  start="$(date +%s%N)"
  "$BIN" "${config_arg[@]}" --db "$DB" --path "$ROOT" top 10 >/dev/null
  end="$(date +%s%N)"
  echo $(( (end - start) / 1000000 )) >>"$tmp"
done
sort -n "$tmp" >"${tmp}.sorted"
avg="$(awk '{s+=$1} END {if (NR) printf "%.1f", s/NR; else print 0}' "${tmp}.sorted")"
p95_line=$(( (RUNS * 95 + 99) / 100 ))
p95="$(sed -n "${p95_line}p" "${tmp}.sorted")"
min="$(head -1 "${tmp}.sorted")"
max="$(tail -1 "${tmp}.sorted")"
echo "查询耗时(ms): avg=$avg min=$min p95=$p95 max=$max"
echo "目标：<100ms；若超过，检查数据库是否在本地盘、是否被其他写入阻塞。"
rm -f "${tmp}.sorted"

cat <<'NOTE'

== 扫描资源观察（另一个终端执行） ==
  pidstat -p "$(pgrep -f 'dirspace daemon')" 5 12
  systemctl show dirspace -p CPUUsageNSec -p MemoryCurrent -p TasksCurrent
  cat /sys/fs/cgroup/system.slice/dirspace.service/cpu.stat
  cat /sys/fs/cgroup/system.slice/dirspace.service/memory.current
  nfsstat -c 5
  cat /proc/self/mountstats

== 可选：实际扫描（会读业务 NFS 元数据，需维护窗口） ==
  PERF_ALLOW_SCAN=1 PERF_ROOT=/mnt bash tests/perf/run.sh
NOTE

if [[ "${PERF_ALLOW_SCAN:-0}" == "1" ]]; then
  echo
  echo "== 扫描一次（/usr/bin/time -v） =="
  /usr/bin/time -v "$BIN" "${config_arg[@]}" --db "$DB" scan --path "$ROOT"
fi
