# dirspace

`dirspace` 是一个面向 Linux 的目录空间扫描与缓存服务，适合 NFS、CFS 和其他共享文件系统。它只读取文件元数据，统计扫描根目录下**一级子目录**的大小、文件数、子目录数、耗时、状态和准确性，结果写入本地 SQLite。日常查询只读本地缓存，不访问共享存储。

GitHub: <https://github.com/YEDEDD/dirspace>

## 特性

- 只读元数据，不读取文件内容，不写入扫描根目录
- 一级目录聚合，适合“哪个目录占了多少空间”的日常查询
- 本地 SQLite WAL 缓存，查询不访问 NFS
- 全局 QPS 限速、CPU/内存保护、NFS 延迟自适应暂停
- 一级目录之间、单个目录内的文件元数据并行，总量受 profile 和 QPS 限制
- 可选挂载点保护，避免 NFS/CFS 未挂载时误扫本地盘
- 支持暂停、恢复、取消、断点续扫和单目录重算
- 失败目录保留上次有效数据；不完整扫描不会覆盖正式缓存
- 单个静态二进制，支持 systemd 和 JSON 输出

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
sudo install -D -m 0755 bin/dirspace /usr/local/bin/dirspace
sudo install -D -m 0640 configs/config.yaml /etc/dirspace/config.yaml
sudo install -d -m 0700 /var/lib/dirspace
```

使用仓库安装脚本时：

```bash
make build-linux
sudo DIRSPACE_BINARY=bin/dirspace-linux-amd64 scripts/install.sh
```

### 首次扫描

```bash
dirspace config explain
dirspace scan --path /mnt
dirspace top 20
dirspace status
```

`config explain` 只读取配置和挂载信息，不需要数据库存在，适合先确认最终生效值。

### 每周定时

持续 daemon 会按 `scan.interval` 刷新。若只想每周跑一次，推荐用 systemd timer，而不是常驻 daemon：

```ini
# /etc/systemd/system/dirspace-scan.service
[Unit]
Description=dirspace weekly scan
RequiresMountsFor=/mnt

[Service]
Type=oneshot
ExecStart=/usr/local/bin/dirspace scan --config /etc/dirspace/config.yaml
Nice=19
CPUQuota=20%
MemoryMax=512M
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

使用 timer 时不要同时启用 `dirspace.service`，避免两套调度重复扫描。使用 `profile: fast` 时，记得同步放宽 unit 的 `CPUQuota` 和 `MemoryMax`，否则 cgroup 会先于应用限流生效。

## 配置

最少只需要三行：

```yaml
profile: normal
scan:
  root_path: /mnt
  interval: 24h
```

生产环境建议再加一行挂载保护：

```yaml
scan:
  require_mount_point: true
```

开启后，`root_path` 不是挂载点就拒绝扫描，避免 CFS/NFS 没挂上时把本地盘当成业务盘扫。它只检查本机挂载表，不检查 NFS 服务端是否健康。

### profile

| profile | QPS 上限 | worker 上限 | CPU | 内存 | 适用 |
|---|---:|---:|---:|---:|---|
| `low` | 100 | 4 | 10% | 256MB | 生产优先，尽量不影响业务 |
| `normal` | 300 | 8 | 20% | 512MB | 默认，日常使用 |
| `fast` | 1000 | 16 | 40% | 1GB | 维护窗口，尽快扫完 |

profile 只是基线。`performance` 下填写的字段会覆盖 profile 中的同名值。

### 自动推导

- `concurrency: auto` 表示“最多使用 profile 的 worker 上限”；一级目录之间并行，单个目录内的文件 `lstat` 也并行。
- 单个一级目录很大时，会从总并发里分到更多内部 worker；一级目录很多时，worker 主要分给目录之间。
- QPS 从上限的约 1/3 起步，延迟和错误正常时逐步上升，有压力时自动下降。
- `max_load1` 默认 0，即不按本机 load 判断共享存储压力；需要时再打开。
- `database.path` 默认 `/var/lib/dirspace/usage.db`，不在配置中填写也可以。
- daemon 每次扫描完成后会按 `database.retention_days` 自动清理旧历史；当前缓存不会被删。

### 高级覆盖

```yaml
performance:
  concurrency: auto        # auto 或固定数字，例如 8
  max_metadata_qps: 500
  max_cpu_percent: 20
  memory_limit: 512MB
  nfs_slow_latency: 500ms
  nfs_pause_latency: 5s
  nfs_pause_cooldown: 30s
  max_metadata_errors: 8
```

`configs/config.yaml` 里有完整注释和可选字段。不要把数据库放在 NFS 上，也不要放在扫描根目录内。

从 1.0 升级：`scan.full_scan_interval` 已删除，旧配置里删掉这一行即可；默认 profile 变为 `normal`，生产环境可先改成 `low` 试跑。

## 扫描耗时

扫描是**全量元数据遍历**，没有一个固定的“几分钟”。先看公式：

```text
耗时 ≈ (文件数 + 目录数 × 2) / 有效元数据 QPS
有效 QPS ≈ min(配置 QPS, 并发数 / 平均元数据延迟)
```

按 1 个文件 1 次 `lstat`、每个目录约 2 次目录操作估算，不含网络抖动和 NFS 客户端缓存。目录越多、层级越深，实际越慢。

| 文件数 | 100 QPS | 300 QPS | 1000 QPS |
|---:|---:|---:|---:|
| 100 万 | 约 2.8 小时 | 约 56 分钟 | 约 17 分钟 |
| 1700 万 | 约 47 小时 | 约 15.7 小时 | 约 4.7 小时 |
| 2000 万 | 约 55.6 小时 | 约 18.5 小时 | 约 5.6 小时 |

上表只按文件数粗算，目录另加约 10%–20%。这是 QPS 打满时的下限；自适应限速会从约 1/3 QPS 起步，所以前几分钟更慢。若 NFS 元数据延迟是 20ms、并发 8，实际上限约 `8 / 0.02 = 400 ops/s`，即使配置 1000 QPS，2000 万文件也要约 14 小时。首次扫描和后续刷新都一样是元数据全量遍历；查询 `top` / `list` 只读本地 SQLite，不访问 NFS。实际扫描结束后会打印“扫描耗时”和“文件吞吐”，可以用实测值替换公式里的估算。

## 命令

| 命令 | 作用 |
|---|---|
| `dirspace` / `dirspace top 20` | 查看一级目录空间排名 |
| `dirspace list` | 查看全部一级目录 |
| `dirspace status` | 查看缓存、任务、进度和数据库状态 |
| `dirspace config explain` | 查看最终生效配置和推导值 |
| `dirspace scan` | 建立基线 |
| `dirspace refresh` | 刷新缓存 |
| `dirspace rebuild <目录>` | 只重算一个一级目录 |
| `dirspace pause/resume/cancel` | 控制当前扫描 |
| `dirspace daemon` | 常驻定时调度 |
| `dirspace cleanup` | 清理历史统计 |
| `dirspace backup` | 备份 SQLite |
| `dirspace doctor` | 检查或修复数据库 |

常用全局参数：`--path`、`--config`、`--db`、`--json`、`--logical`、`--resume`、`--once`。

## 语义与限制

- **只统计一级子目录**；更深层文件递归汇总到一级目录。
- 默认按 `allocated`（`st_blocks × 512`）统计，接近 `du`；`--logical` 使用 `st_size`。
- **每轮刷新都是全量元数据遍历**。没有安全的 NFSv3 增量；不要依赖目录 mtime 跳过读取。
- NFS 客户端有属性缓存，结果不是实时值；从服务端删除的目录可能暂时保留旧行。
- 硬链接只计一次，归属目录在并发扫描下可能变化；总量准确，单目录可能有偏差。
- 扫描失败或取消不会发布半成品；失败目录保留上次有效数据。
- 查询只读 SQLite，所以 NFS 断开时历史结果仍可查看。
- 这是容量审计工具，不是实时监控，也不是文件内容扫描器。

## 开发与许可

```bash
make fmt-check
make vet
make test
make test-race
```

性能验证脚本：`bash tests/perf/run.sh`。

MIT License，见 [LICENSE](LICENSE)。
