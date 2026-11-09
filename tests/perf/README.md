# 性能与资源验证计划

本目录的脚本只做只读查询测试；扫描测试必须显式打开 `PERF_ALLOW_SCAN=1`，并且只能在测试挂载或维护窗口执行。

## 1. 查询延迟

目标：`top 10` p95 < 100ms，且查询不产生 NFS 元数据请求。

```bash
make build-linux
PERF_ROOT=/mnt \
PERF_DB=/var/lib/dirspace/usage.db \
PERF_CONFIG=/etc/dirspace/config.yaml \
PERF_RUNS=50 \
bash tests/perf/run.sh
```

输出示例：

```
查询耗时(ms): avg=3.2 min=2 p95=5 max=9
```

同时用 strace 验证没有访问 NFS：

```bash
strace -f -e trace=openat,newfstatat,statx \
  ./bin/dirspace --config /etc/dirspace/config.yaml \
  --db /var/lib/dirspace/usage.db --path /mnt top 10 \
  2>&1 | grep -E '/mnt|usage.db'
```

允许出现 `usage.db`、`/proc/self/mounts`；不应出现 `/mnt/...` 业务路径。

## 2. 扫描资源

启动扫描后另开终端：

```bash
pidstat -p "$(pgrep -f 'dirspace daemon')" 5 12

systemctl show dirspace \
  -p CPUUsageNSec -p MemoryCurrent -p TasksCurrent \
  -p CPUQuotaPerSecUSec -p MemoryMax

cat /sys/fs/cgroup/system.slice/dirspace.service/cpu.stat
cat /sys/fs/cgroup/system.slice/dirspace.service/memory.current
cat /sys/fs/cgroup/system.slice/dirspace.service/memory.events

nfsstat -c 5
cat /proc/self/mountstats
```

验收：

| 项目 | 目标 |
|---|---|
| cgroup CPU | 不超过 `CPUQuota=20%` |
| RSS / memory.current | < 512MB，无 OOM |
| 应用元数据 QPS | <= `max_metadata_qps` |
| NFS 客户端 ops/s | 与配置量级一致，无重试风暴 |
| 业务 Pod P99 | 无明显劣化（最终标准） |
| 查询 | 仍可 < 100ms，不被扫描事务长时间阻塞 |

## 3. 实际扫描（可选，谨慎）

```bash
PERF_ALLOW_SCAN=1 \
PERF_ROOT=/mnt/test-pvc \
PERF_DB=/var/lib/dirspace/usage.db \
PERF_CONFIG=/etc/dirspace/config.yaml \
bash tests/perf/run.sh
```

脚本会执行一次 `scan --path $PERF_ROOT` 并打印 `/usr/bin/time -v` 的 max RSS、CPU、elapsed。

**不要**在生产业务高峰直接对 `/mnt` 做首次全量。先用测试 PVC，观察：

- `max_metadata_qps` 是否真的限制住 NFS 客户端 ops；
- 服务端 `nfsstat -s` 的 read/write/metadata 延迟；
- 业务延迟；
- 内存是否有增长趋势。

## 4. 极端场景清单

- 100 万 / 1000 万小文件；
- 深层目录（超过 512 层会报错，见 scanner `maxDepth`）；
- 大量硬链接；
- 稀疏文件；
- 扫描过程中持续增删文件；
- NFS 超时、ESTALE、服务端重启；
- Ctrl-C、SIGKILL、`systemctl stop`；
- 两个进程同时扫同一根（应被 flock 拒绝）；
- 数据库磁盘满、数据库损坏；
- `MemoryMax` 压到 128M（观察 OOM 行为）；
- `CPUQuota` 压到 1%（观察是否持续运行而不是崩溃）。

## 5. QPS 与时间估算

应用侧 QPS 是全局 token bucket，容量 1：

```
预计时间 ≈ (文件数 + 目录数 + readdir 批次数 + 重试) / max_metadata_qps
```

50 QPS 下：

| 文件数 | 纯文件时间 | 备注 |
|---:|---:|---|
| 100 万 | ~5.6h | 加目录/readdir 会更久 |
| 1000 万 | ~56h | 默认 24h 间隔不可能完成 |

不要为了“看起来快”直接改大 QPS。先确认 NFS 服务端、网络和业务 Pod 的余量。

## 6. 通过标准

- 查询 p95 < 100ms；
- 查询不访问业务 NFS 路径；
- cgroup CPU 不超限，RSS < 512MB；
- 扫描期间业务 P99 无明显变化；
- 中断/失败不覆盖旧数据；
- `status` 能明确显示 fresh/stale/partial/failed/scanning。
