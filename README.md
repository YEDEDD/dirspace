# dirspace

`dirspace` 是一个面向 Linux 的目录空间扫描与缓存服务，适合 NFS、CFS、Lustre 等共享文件系统。它只读文件元数据，统计扫描根目录下**一级子目录**的空间、文件数、子目录数、耗时、状态和准确性，结果写入本地 SQLite。日常查询只读本地缓存，不访问共享存储。

GitHub: <https://github.com/YEDEDD/dirspace>

## 特性

- 只读元数据，不读文件内容，不写扫描根目录
- 一级目录聚合，直接回答“哪个目录占了多少空间”
- 本地 SQLite WAL 缓存，查询不访问共享存储
- **精确全量**与**快速抽样估算**双模式：精确每周一次，快速默认每 6 小时一次
- 递归目录树并行：一级目录、子目录、文件 `lstat` 共用工作队列
- `scan --new` 只扫新增或失效的一级目录，不重扫已有的
- 已删除目录默认从排名中隐藏，并按 `stale_retention_days` 清理
- 全局限速、CPU/内存保护、共享文件系统延迟自适应暂停
- 支持暂停、恢复、取消、断点续扫和单目录重算
- `status` 显示活动任务实时已运行时间；进程被杀后 90 秒自动恢复为中断状态，不会一直显示“扫描中”
- 失败目录保留上次有效数据；半成品不覆盖正式缓存
- 单个静态二进制，支持 systemd、JSON、ETA 和慢目录耗时展示

## 快速开始

### 构建

```bash
make build            # 当前平台
make build-linux      # Linux amd64
make build-linux-arm64
```

需要 Go 1.27+。也可以直接使用 Release 中的 Linux 二进制：

<https://github.com/YEDEDD/dirspace/releases>

### 安装

```bash
make build-linux
sudo DIRSPACE_BINARY=bin/dirspace-linux-amd64 scripts/install.sh
```

安装脚本会安装二进制、配置、systemd 单元和数据目录，默认启用并启动 `dirspace.service`。已有配置和数据库不会被覆盖。

### 首次使用

```bash
dirspace config explain       # 先看最终配置、挂载和自动推导值
dirspace quick                # 先跑快速估算，几分钟到几小时出近似结果
dirspace scan                 # 再跑精确全量，建立准确基线
dirspace top 20               # 查询只读 SQLite
dirspace status               # 进度、ETA、慢目录、数据库
```

`quick` 和 `scan` 都可以加 `--path /真实挂载点`。不写时使用 `scan.root_path`。

## 定时策略

推荐直接使用常驻 daemon，它同时维护两套调度：

```yaml
scan:
  interval: 168h          # 精确全量：每周
  quick_interval: 6h      # 快速估算：每 6 小时
  quick_sample_percent: 2 # 抽 2% 文件
```

- 快速估算先跑，先给用户一个近似视图
- 精确全量按 `interval` 跑，覆盖近似值
- 只有快速结果时，`accuracy=sampled`；只有精确结果时，`accuracy=exact`
- 两套任务串行执行，不会同时扫描同一个根
- 每次完成后自动清理历史、已删除目录和过期数据

如果只想每周跑一次精确任务，也可以使用 systemd timer，但不要再同时启用 `dirspace.service`：

```ini
# /etc/systemd/system/dirspace-scan.service
[Unit]
Description=dirspace weekly exact scan
RequiresMountsFor=/mnt

[Service]
Type=oneshot
ExecStart=/usr/local/bin/dirspace scan --config /etc/dirspace/config.yaml
Nice=19
CPUQuota=100%
MemoryHigh=12%
MemoryMax=20%
```

```ini
# /etc/systemd/system/dirspace-scan.timer
[Unit]
Description=Run dirspace every Sunday at 02:00

[Timer]
OnCalendar=Sun 02:00
Persistent=true
RandomizedDelaySec=5m

[Install]
WantedBy=timers.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now dirspace-scan.timer
```

## 配置

最小配置：

```yaml
profile: normal

scan:
  root_path: /mnt
  interval: 168h
  quick_interval: 6h
  quick_sample_percent: 2
  require_mount_point: true
```

生产环境建议加 `require_mount_point: true`，避免挂载没就绪时误扫本地盘。

### profile

| profile | QPS 上限 | worker 上限 | CPU | 内存默认值 | 适用 |
|---|---:|---:|---:|---:|---|
| `low` | 100 | 4 | 10% | auto | 生产优先 |
| `normal` | 300 | 8 | 20% | auto | 默认 |
| `fast` | 1000 | 16 | 40% | auto | 维护窗口 |

profile 只是基线，`performance` 下的字段可以覆盖它。三个 profile 的内存默认都是 `auto`：优先按 cgroup 上限的 80% 推导，没有 cgroup 时按物理内存 10%（256MB–4GB）。

### 自动推导

- `concurrency: auto`：按实际一级目录数分配 worker，不再固定写死。
- 递归目录树并行：子目录和文件批次都进入同一队列，深目录不再被单个线程拖住。
- QPS 从上限约 1/3 起步，压力正常时逐步升到上限。
- `memory_limit: auto`：读取 cgroup 内存上限，取 80%；没有 cgroup 时按物理内存 10%，最低 256MB。
- systemd 单元默认用 `MemoryHigh=12%`、`MemoryMax=20%`、`CPUQuota=100%`，按整机自动缩放，不再写死 512M。百分比内存限制需要 systemd 231+；旧系统请改回固定值。
- `CPUQuota` 的百分比是**单核**的百分比，100% 表示最多一个 CPU 核。
- `max_load1` 默认 0，不按本机 load 判断共享存储压力。

### 已删除目录与历史

- 扫描时发现目录已删除，旧行标记为 `stale`，不会立刻删除，避免短暂挂载异常造成数据丢失
- `top` / `list` 默认隐藏 `stale` 行，`status` 会显示隐藏行数
- `database.stale_retention_days` 默认 7 天，daemon 每次扫描后自动清理
- `database.retention_days` 默认 90 天，控制历史统计

### 高级覆盖

```yaml
performance:
  concurrency: auto
  max_metadata_qps: 500
  max_cpu_percent: 20
  memory_limit: auto      # 或 512MB
  nfs_slow_latency: 500ms
  nfs_pause_latency: 5s
  nfs_pause_cooldown: 30s
  max_metadata_errors: 8

database:
  stale_retention_days: 7
```

不要把数据库放在共享存储上，也不要放在扫描根目录内。

## 扫描耗时

精确全量仍然是**全量元数据遍历**，没有安全的通用增量。先看公式：

```text
精确耗时 ≈ (文件数 + 目录数 × 2) / 有效元数据 QPS
有效 QPS ≈ min(配置 QPS, 并发数 / 平均元数据延迟)
```

| 文件数 | 100 QPS | 300 QPS | 1000 QPS |
|---:|---:|---:|---:|
| 100 万 | 约 2.8 小时 | 约 56 分钟 | 约 17 分钟 |
| 1700 万 | 约 47 小时 | 约 15.7 小时 | 约 4.7 小时 |
| 2000 万 | 约 55.6 小时 | 约 18.5 小时 | 约 5.6 小时 |

这是 QPS 打满时的下限；自适应限速会从约 1/3 起步。

**快速估算**只对一部分文件做 `lstat`，但仍要遍历目录树：

```text
快速耗时 ≈ (目录操作 + 抽样文件数) / 有效 QPS
```

它适合回答“哪个目录大、大概多少”，不适合计费或精确对账。抽样结果会标记为 `sampled`，不会被当成精确值。

50T 全量 50 小时，意味着单机容量审计已经到瓶颈：70T 如果文件数增加约 16.7%，接近 58 小时；如果只是大文件变大，可能仍是 50 小时。500T 单机精确全量不可行，需要多客户端分片（当前版本未实现），或者接受快速估算。

## 命令

| 命令 | 作用 |
|---|---|
| `dirspace` / `dirspace top 20` | 一级目录空间排名，显示数据时间、状态、耗时 |
| `dirspace list` | 全部可见一级目录 |
| `dirspace status` | 缓存、当前任务、预计剩余、隐藏 stale 行、数据库 |
| `dirspace config explain` | 最终生效配置和自动推导值 |
| `dirspace quick` | 快速抽样估算，标记 `sampled` |
| `dirspace scan` | 精确全量扫描 |
| `dirspace scan --new` | 只扫新增/失效的一级目录，需要已有基线 |
| `dirspace refresh` | 精确刷新（全量） |
| `dirspace rebuild <目录>` | 只重算一个一级目录 |
| `dirspace pause/resume/cancel` | 控制当前扫描 |
| `dirspace daemon` | 常驻调度：快速 + 精确 |
| `dirspace cleanup` | 清理历史、stale 行，可选 VACUUM |
| `dirspace backup` | 备份 SQLite |
| `dirspace doctor` | 检查或修复数据库 |

常用参数：`--path`、`--config`、`--db`、`--json`、`--logical`、`--full`、`--new`、`--resume`、`--once`。

## 语义与限制

- **只统计一级子目录**；更深层文件递归汇总到一级目录。
- 默认按 `allocated`（`st_blocks × 512`）统计，接近 `du`；`--logical` 使用 `st_size`。
- **精确全量每轮都遍历全部元数据**。没有可靠的 NFS/Lustre 客户端增量，目录 mtime 和 inotify 都不可靠。
- `scan --new` 只处理新增或非 fresh 的一级目录，不检查已有目录内部的变化；结果标记为近似。
- 快速估算的字节数是抽样推算，目录块和文件数是精确的；硬链接不做去重，可能轻微高估。
- 共享文件系统客户端有属性缓存，结果不是实时值；删除的目录可能短暂显示为 stale，随后隐藏并清理。
- 硬链接在精确模式下只计一次，归属目录在并发扫描下可能变化；总量准确，单目录可能有偏差。
- 扫描失败或取消不发布半成品；失败目录保留上次有效数据。
- 查询只读 SQLite，共享存储断开时历史结果仍可查看。
- 这是容量审计/容量视图工具，不是实时监控，也不是文件内容扫描器。
- 500T 级别且要求精确、低延迟时，单机全量不是正确架构；需要多客户端分片，当前版本没有实现。

## 开发与许可

```bash
make fmt-check
make vet
make test
make test-race
```

性能验证脚本：`bash tests/perf/run.sh`。

MIT License，见 [LICENSE](LICENSE)。
