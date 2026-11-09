# dirspace

`dirspace` 是一个面向 NFS / 共享文件系统的目录空间扫描与缓存服务，运行在 Linux 客户端上。它只读取文件元数据，统计扫描根目录下**一级子目录**的空间、文件数、子目录数、时间、耗时、状态和准确性，并把结果缓存在本地 SQLite。日常查询只读 SQLite，不访问 NFS。

GitHub: <https://github.com/YEDEDD/dirspace>

> **交付状态**：本仓库包含完整可编译的 Go 实现、SQLite schema、CLI、daemon、systemd 单元、安装/卸载脚本、测试和性能验证脚本。
> 已在开发机（macOS/APFS）通过 `go test ./...`、`go test -race ./...` 和 Linux amd64/arm64 交叉编译；
> **尚未在真实生产 NFS 集群上做压测**。默认参数刻意保守，上线前请按本文的性能验证计划在自己的维护窗口验证。

---

## 目录

- [1. 目标与非目标](#1-目标与非目标)
- [2. 功能](#2-功能)
- [3. 架构与关键技术选型](#3-架构与关键技术选型)
- [4. 项目结构](#4-项目结构)
- [5. 编译](#5-编译)
- [6. 安装与卸载](#6-安装与卸载)
- [7. 命令](#7-命令)
- [8. 配置文件](#8-配置文件)
- [9. systemd 资源硬限制](#9-systemd-资源硬限制)
- [10. 扫描与缓存一致性](#10-扫描与缓存一致性)
- [11. 准确性、口径与边界](#11-准确性口径与边界)
- [12. 关于增量刷新](#12-关于增量刷新)
- [13. 资源保护如何落地](#13-资源保护如何落地)
- [14. 首次扫描、日常查询与故障恢复](#14-首次扫描日常查询与故障恢复)
- [15. 测试](#15-测试)
- [16. 性能验证计划](#16-性能验证计划)
- [17. 故障排查](#17-故障排查)
- [18. 安全与权限](#18-安全与权限)
- [19. 已知限制与后续方向](#19-已知限制与后续方向)
- [20. License](#20-license)

---

## 1. 目标与非目标

### 目标

- 在 Kubernetes 节点或普通 Linux 主机上统计 NFS 挂载目录的空间占用。
- 百万、千万级小文件不把整个目录树读进内存。
- 对 NFS 元数据请求做全局限速，默认 `50 ops/s`，尽量不影响生产。
- 查询命令只读本地 SQLite，目标 `<100ms`，NFS 暂时不可用时仍能看到上次缓存。
- 统计失败、扫描中断、进程被杀时不覆盖上一次有效数据，不把失败目录写成 0。
- 提供可运维的 CLI、daemon、systemd 单元、暂停/恢复/取消、断点续扫、单目录重算、JSON 输出和数据库维护命令。

### 非目标

- **不是实时监控**。NFSv3 客户端无法可靠获得服务端变更通知；结果是客户端元数据快照。
- 不读取文件内容，不做 hash，不修改、不迁移、不删除业务文件。
- 不替代 NFS 服务端配额、`du` 审计或存储厂商的监控。
- 不承诺“查询结果等于 NFS 服务端实时占用”。所有输出都带客户端快照说明。
- 不通过目录 mtime、inotify 或“文件清单没变”来跳过 NFS 元数据读取，从而伪造增量准确性。

---

## 2. 功能

- 默认扫描根 `/mnt`，支持 `--path` 指定任意绝对路径。
- 支持 YAML 配置多个扫描根；每个根有独立的根记录、任务、缓存行和锁。
- 只输出一级子目录；更深层文件递归汇总到所属一级目录。
- 根目录直属文件、根目录自身的目录块也计入根合计（不是一级子目录行）。
- 空间按 `st_blocks × 512`（默认，接近 `du`）或 `st_size`（`--logical`）统计。
- 同一 inode 的硬链接只计一次大小；文件数按路径数统计。
- 不跟随符号链接；符号链接只计入 `symlink_count`，不计入文件数和目标大小。
- 首次全量基线、后台定时刷新、手动刷新、单目录重算。
- 暂停、恢复、取消；Ctrl-C / SIGTERM 标记中断；`scan --resume` 断点续扫。
- 失败目录保留上次有效字节和状态，标记 stale，绝不覆盖为 0。
- 只有完整跑完并通过事务提交才替换正式缓存；中断/取消/致命错误不发布半成品。
- 查询、状态、JSON、TOP N、全部目录、历史清理、备份、完整性检查、损坏重建。
- systemd `Nice=19`、`CPUQuota=10%`、`MemoryMax=512M`、`TasksMax=128`。
- 可选的本地文件清单（默认关闭）；它只增加本地记录，不减少 NFS 元数据读取。

---

## 3. 架构与关键技术选型

### 3.1 分层

```
┌────────────────────────────────────────────────────────────┐
│ CLI: dirspace top/list/status/scan/daemon/...             │
│      - 参数解析、退出码、JSON/表格、信号                      │
└───────────────────────────┬────────────────────────────────┘
                            │
┌───────────────────────────▼────────────────────────────────┐
│ service: 业务编排                                            │
│  - 路径规范化/安全校验、每根 flock、Scan/Rebuild/Query       │
│  - Pause/Resume/Cancel、daemon 串行调度、维护命令             │
└───────────────┬──────────────────────────────┬─────────────┘
                │                              │
┌───────────────▼──────────────┐  ┌────────────▼─────────────┐
│ scanner                      │  │ storage                  │
│  - 根目录发现                 │  │  - SQLite WAL            │
│  - 根直属文件                 │  │  - run_dirs 暂存         │
│  - 一级目录 worker pool       │  │  - 事务原子晋升           │
│  - 流式 ReadDir(256)         │  │  - 失败保留旧值           │
│  - 硬链接去重                  │  │  - 查询/历史/备份         │
│  - 进度/心跳/控制              │  └──────────────────────────┘
└───────────────┬──────────────┘
                │
┌───────────────▼────────────────────────────────────────────┐
│ performance: QPS token bucket / CPU guard / memory guard    │
│              host load / NFS latency pause / adaptive rate   │
└────────────────────────────────────────────────────────────┘
```

### 3.2 为什么是 Go

- 单二进制、无运行时依赖，适合节点上部署。
- goroutine 适合少量 worker + 控制协程，不需要引入线程池框架。
- `os.File.ReadDir(n)` 支持流式读取目录，避免 `os.ReadDir` 一次性分配整个目录。
- 交叉编译简单；`CGO_ENABLED=0` 即可生成 Linux 二进制。

### 3.3 为什么是 SQLite WAL，而不是内存或 JSON

- 千万级文件如果只存在内存，进程重启/被杀就全丢；JSON 无法安全地并发读写，也无法做原子提交。
- SQLite 单文件、本地磁盘、WAL 允许“查询读旧快照、扫描写暂存”，不会让查询读到半成品。
- 用纯 Go 驱动 `modernc.org/sqlite`，`CGO_ENABLED=0` 也能编译，避免节点缺少 libsqlite3。
- 一级目录数量通常远小于文件数；空间占用与文件数基本无关（除非显式开启 inventory）。
- 写连接单连接串行化，读连接独立小池；WAL + `busy_timeout` 降低锁冲突。

### 3.4 为什么查询不访问 NFS

`Service.Query` 只做：

1. 规范化路径；
2. 查 `roots` / `top_dirs`；
3. 读 `/proc/self/mounts` 做来源展示（本地文件，不访问 NFS）。

它不 `stat` 查询路径，不打开目录，不遍历文件。因此 NFS 暂时不可用、目录被卸载、甚至路径被改名，查询仍能返回上次缓存；缓存过期时只显示 `stale`，不会触发隐式扫描。

### 3.5 数据模型

核心表：

| 表 | 用途 |
|---|---|
| `roots` | 每个扫描根的聚合、状态、时间、根直属文件统计 |
| `scan_runs` | 每次扫描任务、进度、心跳、根直属暂存 |
| `run_dirs` | 本次任务已成功/失败的一级目录暂存行 |
| `top_dirs` | 正式发布的一级目录缓存 |
| `dir_history` | 每次成功发布的历史记录，用于保留和排障 |
| `hardlink_seen` | 本次任务已见 `(dev, ino)`，避免硬链接重复计大小 |
| `control` | 跨进程 pause/resume/cancel 控制 |
| `inventory` | 可选文件清单，默认关闭 |

`run_dirs`、`scan_runs`、`hardlink_seen` 是“本次任务”的暂存区；`top_dirs` 是上一次已提交的正式缓存。扫描失败只改状态/时间/错误，不动 `top_dirs` 的字节。

---

## 4. 项目结构

```
dirspace/
├── cmd/dirspace/main.go
├── internal/
│   ├── cli/            # 参数、命令分发、表格/JSON 输出
│   ├── config/         # YAML、默认值、路径与安全校验
│   ├── logger/         # slog 文本/JSON
│   ├── model/          # 领域模型
│   ├── mountinfo/      # /proc/self/mounts 解析（查询安全）
│   ├── performance/    # QPS、CPU、内存、host load、NFS 延迟
│   ├── platform/       # Linux/macOS stat、getrusage、nice
│   ├── scanner/        # 流式扫描、worker、硬链接、inventory
│   ├── service/        # 业务编排、锁、daemon、维护
│   └── storage/        # schema、SQLite、原子提交、查询
├── configs/config.yaml
├── deploy/dirspace.service
├── scripts/install.sh
├── scripts/uninstall.sh
├── tests/perf/run.sh
├── tests/perf/README.md
├── .gitignore
├── go.mod
├── go.sum
├── LICENSE
├── Makefile
└── README.md
```

---

## 5. 编译

要求 Go **1.27+**（`go.mod` 按当前工具链固定；低版本请自行调整依赖版本）。

```bash
# 当前平台
make build
./bin/dirspace --version
./bin/dirspace --help

# 直接使用 go
CGO_ENABLED=0 go build -trimpath -o bin/dirspace ./cmd/dirspace

# Linux amd64（在 macOS/CI 上交叉编译）
make build-linux
file bin/dirspace-linux-amd64
# ELF 64-bit LSB executable, x86-64 ...

# Linux arm64
make build-linux-arm64

# 质量门禁
make check          # gofmt 检查 + go vet + go test
make test-race
```

`modernc.org/sqlite` 是纯 Go 实现，`CGO_ENABLED=0` 可以正常编译和运行。

---

## 6. 安装与卸载

### 6.1 一键安装（Linux + systemd）

```bash
# 在构建机生成 Linux 二进制
make build-linux

# 把仓库和二进制放到目标机，或直接复制 bin/dirspace-linux-amd64
sudo env DIRSPACE_BINARY=/path/to/bin/dirspace-linux-amd64 \
  scripts/install.sh
```

安装脚本会：

- 安装二进制到 `/usr/local/bin/dirspace`；
- 若 `/etc/dirspace/config.yaml` 不存在，复制示例；已存在则保留；
- 创建 `/var/lib/dirspace`（0700），**不覆盖已有数据库**；
- 安装 `/etc/systemd/system/dirspace.service`；
- `systemctl daemon-reload`、`enable`、默认 `restart` 启动；
- 安装 README 到 `/usr/share/doc/dirspace/`。

选项：

```bash
sudo scripts/install.sh --no-start   # 只安装/enable，不启动
```

安装后编辑配置，确认 `scan.root_path` 指向真实挂载：

```bash
sudo vi /etc/dirspace/config.yaml
sudo systemctl restart dirspace
sudo systemctl status dirspace
journalctl -u dirspace -f
```

### 6.2 卸载

```bash
# 默认：停止/禁用服务，删除二进制和 unit，保留配置与 SQLite 历史
sudo scripts/uninstall.sh

# 同时删除配置
sudo scripts/uninstall.sh --purge-config

# 同时删除 SQLite 历史、锁和备份副本（会交互确认）
sudo scripts/uninstall.sh --purge-data
sudo scripts/uninstall.sh --purge-data --yes
```

默认数据路径：

```
/var/lib/dirspace/usage.db
/var/lib/dirspace/usage.db-wal
/var/lib/dirspace/usage.db-shm
/var/lib/dirspace/locks/
```

卸载脚本只操作固定的系统路径，避免变量为空时误删。

---

## 7. 命令

全局参数可以放在命令前或命令后：

```bash
dirspace [全局参数] [命令] [命令参数]
```

### 7.1 查询

```bash
dirspace                       # 等价于 top 10
dirspace top 10
dirspace top 20
dirspace list                  # 全部一级目录
dirspace list --json
dirspace --path /data/cae365-test top 10
dirspace --path /data status
dirspace --path /data/alpha top 20 --json
dirspace --logical top 10      # 用 st_size 展示
```

查询只读 SQLite。指定目录尚未建立基线时，会输出：

```
路径尚未统计: /data/cae365-test，请先执行: dirspace scan --path /data/cae365-test
```

### 7.2 扫描

```bash
# 建立基线；全量元数据遍历
dirspace scan --path /mnt

# 刷新（当前版本每轮都是全量，见第 12 节）
dirspace refresh --path /mnt

# 续扫最近一次被 Ctrl-C/SIGTERM/取消的任务
# 只续扫“最近一次”任务；若它已经完成，则重新全量，不会跳过它去续更旧的暂存
dirspace scan --path /mnt --resume

# 只重算一个已知根下的一级目录
dirspace rebuild --path /mnt/cae365-test
dirspace rebuild cae365-test --path /mnt
```

扫描进度输出到 stderr；`--json` 的最终摘要输出到 stdout，不混在一起。

退出码：

| 退出码 | 含义 |
|---|---|
| `0` | 成功 |
| `1` | 运行错误、未扫描、扫描失败/取消/中断、无活动任务 |
| `2` | 参数/用法错误 |

### 7.3 控制

```bash
dirspace pause  --path /mnt
dirspace resume --path /mnt
dirspace cancel --path /mnt
```

控制是写入 SQLite 的 `control` 表；扫描进程每 1 秒轮询。取消不会发布半成品，已完成的目录暂存保留，可用于后续 `scan --resume`。daemon 的 `resume_interrupted` 只续扫异常中断，不会自动续扫用户主动取消的任务。

### 7.4 daemon

```bash
dirspace daemon --once     # 只跑一轮，便于测试
dirspace daemon            # systemd 长期运行
```

daemon 行为：

- 读取配置中的所有根（`scan.root_path` + `scan.extra_roots`）；
- **串行**扫描，避免多个根同时放大 NFS 压力；
- 首次启动若根未扫描且 `scan_on_start: true`，立即建立基线；
- 其他根按 `interval` 安排下一次；
- 同一进程内不并发扫描；不同进程由每个根的 `flock` 防止重复；
- 收到 SIGTERM 时中断当前扫描，任务标记为 interrupted，缓存保留。

### 7.5 维护

```bash
dirspace status
dirspace status --json
dirspace backup --output /var/backups/dirspace.db
dirspace cleanup --days 90
dirspace cleanup --days 90 --vacuum
dirspace doctor
dirspace doctor --repair
dirspace version
dirspace help
```

- `cleanup` 清理 `dir_history` 和已结束的 `scan_runs`；保留当前 `top_dirs` 缓存。
- `backup` 使用 `VACUUM INTO`，生成一个一致的 SQLite 快照。
- `doctor` 执行 `PRAGMA quick_check`。
- `doctor --repair` 会把损坏的数据库（含 `-wal`/`-shm`）改名保留，并创建空缓存；修复后需要重新扫描。执行前请停服务。

### 7.6 JSON 字段

`list --json` / `top N --json`：

```json
{
  "path": "/mnt/cae365-test",
  "scan_root": "/mnt",
  "single_directory": true,
  "has_baseline": true,
  "size_basis": "allocated",
  "scanned_at": "2026-11-09T07:00:00Z",
  "last_attempt_at": "2026-11-09T07:00:00Z",
  "age_seconds": 12,
  "duration_seconds": 0.42,
  "data_status": "fresh",
  "data_status_text": "正常",
  "accuracy": "exact",
  "accuracy_text": "精确（客户端元数据）",
  "total_bytes": 123456789,
  "total_alloc_bytes": 123456789,
  "total_logical_bytes": 120000000,
  "file_count": 1234,
  "dir_count": 12,
  "directories": [
    {
      "rank": 1,
      "has_baseline": true,
      "name": "alpha",
      "size_bytes": 100000000,
      "size_text": "95.37 MB",
      "alloc_bytes": 100000000,
      "logical_bytes": 99000000,
      "file_count": 100,
      "dir_count": 3,
      "symlink_count": 0,
      "data_status": "fresh",
      "data_status_text": "正常",
      "accuracy": "exact",
      "accuracy_text": "精确（客户端元数据）",
      "scanned_at": "2026-11-09T07:00:00Z",
      "age_seconds": 12,
      "duration_seconds": 0.1
    }
  ],
  "note": "客户端元数据快照，不代表 NFS 服务端实时占用；扫描期间文件可能变化。"
}
```

`total_bytes` 按 `size_basis` 选择 allocated/logical；同时保留 `total_alloc_bytes` 和 `total_logical_bytes`。
`has_baseline: false` 表示没有完整的成功基线：失败的单个目录 `size_bytes`/`file_count` 只是占位 0，而根级 total 可能是已成功目录的部分和；消费方应以 `size_text`/`total_text` 和 `last_success_at` 为准。

---

## 8. 配置文件

默认查找顺序：

1. `--config <file>`（必须存在）；
2. 环境变量 `DIRSPACE_CONFIG`；
3. `/etc/dirspace/config.yaml`；
4. 当前工作目录 `configs/config.yaml`；
5. 都没有则使用内置默认值。

示例见 [`configs/config.yaml`](configs/config.yaml)。完整字段：

```yaml
database:
  path: /var/lib/dirspace/usage.db
  busy_timeout: 5s
  retention_days: 90

scan:
  root_path: /mnt
  extra_roots: []
  interval: 30m
  full_scan_interval: 24h
  scan_on_start: true
  startup_delay: 15s
  resume_interrupted: true
  size_basis: allocated          # allocated | logical
  inventory:
    enabled: false
    batch_size: 1000

performance:
  concurrency: 2
  max_metadata_qps: 50
  min_metadata_qps: 5
  max_cpu_percent: 10
  memory_limit: 512MB
  adaptive_throttle: true
  low_priority: true
  sample_interval: 5s
  max_host_cpu_percent: 85
  max_load1: 8
  nfs_slow_latency: 500ms
  nfs_pause_latency: 5s
  nfs_pause_cooldown: 30s
  max_metadata_errors: 8
  progress_interval: 10s
  heartbeat_interval: 5s

logging:
  level: info                    # debug | info | warn | error
  format: text                   # text | json
```

关键约束（启动时校验）：

- `database.path` 必须是绝对路径，不能是 `/`，不能位于任何扫描根内，不能位于 NFS 挂载上。
- `scan.root_path` 不能是 `/`；`scan.extra_roots` 同样。
- `scan.interval >= 10s`；`full_scan_interval >= interval`（当前版本每轮都全量，该字段预留给未来增量）。
- `concurrency` 1–64；`max_metadata_qps` 1–1,000,000；`min <= max`。
- `inventory.batch_size` 100–2000；写入时会再按 SQLite 参数上限分块。
- `memory_limit >= 32MB`。
- `nfs_pause_latency > nfs_slow_latency`。
- `logging.level` / `format` 只接受列出的值。
- 配置文件使用严格字段解析，写错字段会直接报错，避免“以为改生效了”。

> `database.path` 默认在 `/var/lib/dirspace`。systemd 单元使用 `StateDirectory=dirspace` 并启用 `ProtectSystem=strict`；如果你改数据库路径，必须同步修改 unit 的 `ReadWritePaths`/`StateDirectory`。

---

## 9. systemd 资源硬限制

[`deploy/dirspace.service`](deploy/dirspace.service) 关键项：

```ini
Nice=19
CPUAccounting=true
CPUQuota=10%
CPUWeight=10

MemoryAccounting=true
MemoryHigh=480M
MemoryMax=512M

TasksAccounting=true
TasksMax=128

StateDirectory=dirspace
StateDirectoryMode=0700
UMask=0077

ProtectSystem=strict
ProtectHome=read-only
PrivateTmp=true
NoNewPrivileges=true
```

说明：

- `CPUQuota=10%` 是 cgroup 的硬上限；应用内部的 CPU governor 是第二道软保护。
- `MemoryMax=512M` 是硬上限；应用内的 `debug.SetMemoryLimit(512MB)` 和内存压力暂停是提前量。
- `Nice=19` 降低 CPU 调度优先级。
- `IOWeight=10` 只影响本机块设备调度，**不能限制 NFS 元数据请求**；NFS 保护来自应用内的 QPS/并发/延迟暂停。
- 不要用 `ionice` 代替 NFS 限流。
- `ProtectSystem=strict` 会把 `/mnt`、`/home` 等路径变成只读；扫描本来就是只读。自定义数据库路径时必须给写权限。
- 服务以 root 运行是為了读取所有业务挂载；如果目录权限允许，也可以改成专用用户，但要同步 StateDirectory 和挂载权限。

验证 cgroup：

```bash
systemctl show dirspace -p CPUQuotaPerSecUSec -p MemoryMax -p TasksMax
systemd-cgtop
cat /sys/fs/cgroup/system.slice/dirspace.service/cpu.stat
cat /sys/fs/cgroup/system.slice/dirspace.service/memory.current
```

---

## 10. 扫描与缓存一致性

一次扫描的流程：

1. `Service.Scan` 规范化路径，`Lstat` 根目录并拒绝符号链接；获取每根 `flock`。
2. `storage.StartRun` 创建/恢复任务；根状态变为 `scanning`。
3. 扫描根目录，得到一级子目录列表；按名称排序后放入 worker 队列。
4. 根直属文件单独扫描，写入 `scan_runs` 的 root-self 暂存（不混入一级目录）。
5. worker 递归流式读取每个一级目录：
   - `os.Open` → `ReadDir(256)`；
   - 每个文件 `Lstat`，读取 `st_blocks`/`st_size`/inode/nlink；
   - 硬链接通过 SQLite `(run_id, dev, ino)` 原子 claim，只计一次大小；
   - 目录块计入该一级目录；
   - 符号链接不跟随，只计数；
   - 每完成一个一级目录，写 `run_dirs` 暂存并更新进度/心跳。
6. 所有 worker 结束后，`CompleteRun` 在**一个写事务**里：
   - 成功目录 upsert 到 `top_dirs`；
   - 失败目录只更新 `last_attempt_at`/`last_error`/status，保留旧字节；
   - 本次未出现但以前存在的一级目录标记 stale，保留字节；
   - 根合计 = 所有一级目录 + 根直属文件/目录块；
   - 写历史、清理暂存、设置 control idle。
7. 只有事务提交成功，查询才能看到新数据。

失败保护：

- 单个一级目录失败：该行标记 `stale`，旧 `alloc_bytes/logical_bytes/file_count` 原样保留；根变为 `partial`。
- 首次扫描某目录就失败：插入 `failed` 行，`last_success_at` 为空，查询显示 `n/a`，绝不显示 0。
- 整个扫描失败/取消/中断：不调用 `CompleteRun`，正式缓存完全不变。
- 数据库提交失败：不发布；任务失败，旧缓存保留。
- 断点续扫：已成功的一级目录从 `run_dirs` 恢复，只扫未完成部分；整体标记 `approximate`。
- 目录在扫描期间被删除：该目录标记 stale，旧数据保留；根变 partial。

---

## 11. 准确性、口径与边界

### 11.1 “精确”是什么意思

`accuracy: exact` 只表示：本次遍历没有遇到元数据读取错误、没有断点续扫、没有瞬时消失。它**不表示**：

- 所有文件在同一个时间点被原子快照；
- 结果等于 NFS 服务端实时占用；
- 扫描期间没有新文件写入。

扫描过程中文件可能新增、删除、改大小。`data_status` 和 `accuracy` 是提示，不是服务端保证。

### 11.2 空间计算

默认 `allocated`：

```
目录大小 = 该目录内所有普通文件 st_blocks×512 之和（硬链接按 inode 去重）
         + 该目录自身及其子目录的目录块
```

不含：

- 符号链接目标（符号链接本身通常占 0 块，只计数量）；
- 设备文件、FIFO、socket 等特殊文件；
- NFS 服务端快照、副本、压缩、配额开销；
- 根目录以外的挂载边界（如果一级目录下又挂了别的文件系统，会继续遍历）。

`--logical` 使用 `st_size`，适合看“逻辑大小”，通常与 `du --apparent-size` 更接近，但与 `du` 默认值不同。

### 11.3 硬链接

- 大小：同一 `(dev, inode)` 在一次任务中只计一次。
- 文件数：按路径数统计（与 `find | wc -l` 接近），一个硬链接路径也会 +1。
- 大小归属：由“哪个目录先扫描到”决定；两个一级目录通过硬链接共享大文件时，**根合计正确，但单个目录的字节可能偏向其中一个目录**。并发 worker 下这个归属不保证稳定。
- 根直属文件先于 worker 扫描，所以根直属硬链接会优先归属根合计。
- 单目录重算不重建其他目录的硬链接状态；若目标目录与旧兄弟行共享硬链接，根合计可能多计一次（目标行按本次扫描，旧兄弟行保留旧字节）。这是 `rebuild` 的已知边界。
- 同理，某个一级目录失败并保留旧行时，如果根直属新出现指向该旧行文件的硬链接，根合计也可能多计一次；日常独立文件不受影响。

### 11.4 根目录直属文件

根目录下的普通文件不属于任何一级子目录，但会计入根合计：

```
根 total = Σ 一级目录 + 根直属文件 + 根目录块
```

因此 `dirspace --path /mnt/cae365-test`（当它以 `/mnt` 为扫描根时）显示目标行；如果 `/mnt/cae365-test` 本身是扫描根，则显示其一级子目录，根合计包含直属文件。

### 11.5 NFS 属性缓存

NFS 客户端会缓存属性（`ac`/`actimeo`）。扫描读到的是客户端可见值；服务端刚发生的变更可能延迟可见。工具无法绕过这一点，也不应声称能绕过。

### 11.6 与 `du` 的差异

可能不同：

- `du` 默认按块、但不同文件系统块大小和目录块处理略有差异；
- NFS 服务端可能对目录块、稀疏文件、压缩/去重有不同呈现；
- 硬链接归属；
- 扫描期间的文件变化；
- `du` 会统计特殊文件/符号链接的差异。

在 NFS 上通常接近，但**不要把它当审计级精确值**。

### 11.7 时间与状态

- 数据库时间统一 UTC 存储，CLI 按本地时区显示。
- `last_success_at` 是最近一次成功发布的时间。
- `last_attempt_at` 是最近一次尝试的时间，失败也会更新。
- 超过 `scan.interval` 的 `fresh` 数据在查询时显示 `stale`；这是展示层判定，不修改数据库。
- `partial` / `failed` 不会被“时间过期”覆盖成 `stale`，避免掩盖失败。

---

## 12. 关于增量刷新

**当前版本没有启用“跳过 NFS 元数据”的增量模式。**

原因：

- NFSv3 没有可靠的变更通知；目录 mtime 只在目录项增删时变化，文件内容/大小变化不会触发。
- 仅凭目录 mtime 或“文件清单没变”跳过目录，会把已变化的文件当成旧值，违反准确性要求。
- 即使有文件清单，要确认每个文件是否变化，仍然需要对每个文件做一次 `stat`，NFS 元数据 QPS 并不会减少；收益主要是本地写放大，而不是 NFS 读取。
- 因此 `refresh` 和定时 daemon **每轮都是全量元数据遍历**，只是受 QPS/CPU/延迟保护。`full_scan_interval` 字段预留给未来的安全增量实现，当前不改变行为。

`scan.inventory.enabled: true` 时，工具会额外写本地 `inventory` 表（文件清单），用于排障/未来扩展；它**不会降低 NFS 元数据读取量**，默认关闭。

如果后续实现真正安全的增量，必须满足：

- 有可靠的服务端变更信号（如 NFSv4 change attribute、厂商 API），或
- 对所有文件做元数据校验后再决定，且明确说明 QPS 不下降；
- 不能把 missing/stale 伪装成 fresh；
- 不能因为跳过目录而把旧值当新值发布。

---

## 13. 资源保护如何落地

### 13.1 QPS

`performance.Throttle` 是容量为 1 的 token bucket：

- `max_metadata_qps` 是全局上限，不是每 worker；
- 每次 `open`、每批 `readdir`、每个文件 `lstat` 都先 `Wait`；
- 限速是真实代码，不是只写配置；
- 并发 2 只是允许两个 worker，但总 token 速率仍受全局限制。

实际 NFS RPC 次数可能高于/低于 token 数，因为客户端会合并 readdir、缓存属性、重试；应用侧保证自己发起的元数据操作不超过配置速率。

### 13.2 CPU

- 应用内 `CPUGuard` 每 ≥250ms 用 `getrusage` 计算进程 CPU/wall，超过 `max_cpu_percent` 就 sleep，把平均 CPU 拉回目标附近；单次最多睡 2s。
- systemd `CPUQuota=10%` 是硬限制。
- `Nice=19` 降低调度优先级。
- 主机 CPU/load 超阈值时自适应降低 QPS（`max_host_cpu_percent`、`max_load1`）。

### 13.3 内存

- 目录用 `File.ReadDir(256)` 流式读取，不一次性加载整棵树。
- worker 数默认 2；job channel 无缓冲，不会堆积目录任务。
- SQLite 使用小 cache（`cache_size(-8192)`）。
- `debug.SetMemoryLimit(512MB)` + 采样 `HeapAlloc` 超过 85% 时暂停并 GC。
- systemd `MemoryMax=512M` / `MemoryHigh=480M` 硬兜底。
- inventory 默认关闭；开启时用有界 channel + 批量写。

注意：`MemoryMax` 是硬上限，达到时内核可能 OOM kill；`MemoryHigh` 会节流。应用内限制是提前量，不是绝对 RSS 控制。

### 13.4 NFS 延迟与错误

- 每次元数据操作记录耗时，EWMA 超过 `nfs_slow_latency` 时自适应降速。
- 单次超过 `nfs_pause_latency` 时自动暂停 `nfs_pause_cooldown`。
- 30 秒内网络类错误（ETIMEDOUT/ESTALE/EIO/ENOTCONN/ECONNRESET/EHOSTUNREACH/ENETUNREACH 等）达到 `max_metadata_errors` 时暂停。
- 暂停期间 `Wait` 循环，不产生 NFS 请求；冷却后自动恢复。
- 查询不受影响，继续读缓存。

### 13.5 默认值的代价

默认 `50 ops/s` 非常保守：

| 文件数 | 纯文件元数据理论时间（50/s） | 说明 |
|---:|---:|---|
| 100 万 | ~5.6 小时 | 还要加目录/readdir/重试 |
| 1000 万 | ~56 小时 | 默认 24h 全量间隔不可能完成 |

因此：

- 千万级目录首次基线请安排在维护窗口，或临时提高 `max_metadata_qps`（确认 NFS 服务端余量）。
- `scan.interval` 默认 30m，但一次扫描本身可能数小时；daemon 是串行的，扫描结束后才安排下一次，不会重叠。请把 `interval` 设置为大于单次扫描时长 + 余量。
- `full_scan_interval` 当前不影响行为（每轮全量）；不要误以为它能让 10M 文件在 24h 内完成。

---

## 14. 首次扫描、日常查询与故障恢复

### 14.1 上线顺序

```bash
# 1. 小目录试跑：确认权限、QPS、日志
dirspace scan --path /mnt/test-pvc

# 2. 查看结果
dirspace --path /mnt/test-pvc list
dirspace --path /mnt/test-pvc status

# 3. 正式根建立基线（维护窗口）
dirspace scan --path /mnt

# 4. 安装/启动 daemon
sudo systemctl start dirspace
```

### 14.2 中断与续扫

```bash
dirspace scan --path /mnt
# Ctrl-C
dirspace status --path /mnt          # 最近任务：已中断
dirspace scan --path /mnt --resume   # 继续，整体 approximate
```

进程被 SIGKILL 时，`status` / daemon 启动会调用 `RecoverStaleRuns`，心跳超过 90 秒的 running/paused 任务标记 interrupted，控制状态回 idle。

### 14.3 NFS 不可用

- 查询：继续返回缓存，状态可能 stale/partial/failed；不访问 NFS。
- 扫描：打开/读取失败会记录，单目录失败保留旧值；根级不可用则任务失败，缓存不动。
- daemon：本轮失败，等待下一轮；不会因为 NFS 短暂抖动清空数据。

### 14.4 取消

```bash
dirspace cancel --path /mnt
```

取消后：

- 正式 `top_dirs` 不变；
- `scan_runs` 标记 canceled，`run_dirs` 暂存保留；
- 可以 `scan --resume` 继续（续扫数据标记 approximate）；
- 控制状态回 idle。

### 14.5 单目录重算

```bash
dirspace rebuild --path /mnt/cae365-test
```

只读取该目录，不触碰兄弟目录和根直属统计；根标记 partial（因为不是完整根刷新），目标行更新为 fresh。失败时目标保留旧值。

---

## 15. 测试

```bash
make test
make test-race
make vet
make fmt-check
```

覆盖：

- 配置解析、字节/时长、路径安全、未知字段拒绝。
- SQLite 初始化、v1→当前版本迁移、根隔离、硬链接 claim、失败保留、目录消失 stale、备份/清理、断点暂存恢复。
- 扫描：空根、硬链接跨目录、符号链接不跟随、根直属文件、单目录重算、目录消失、暂停/恢复/取消。
- 服务：查询不访问文件系统（扫描后把根移走仍可查询）、多根隔离、状态、符号链接根拒绝。
- CLI：端到端扫描/查询/JSON/status/rebuild/backup/doctor、用法错误、版本帮助。
- 性能：token bucket 速率和取消、暂停生效、CPU guard 关闭路径。

测试不依赖真实 NFS。生产前必须按下一节在真实 NFS 上验证。

---

## 16. 性能验证计划

### 16.1 查询延迟（不碰 NFS）

```bash
make build-linux
PERF_ROOT=/mnt PERF_DB=/var/lib/dirspace/usage.db \
  PERF_CONFIG=/etc/dirspace/config.yaml \
  bash tests/perf/run.sh
```

脚本默认跑 50 次 `top 10`，输出 avg/min/p95/max。验收：

- p95 **< 100ms**（本地 SSD、数据库未被写事务长时间阻塞）；
- 查询期间 `strace -f -e trace=openat,newfstatat,statx` 不应出现 `/mnt` 下的业务路径（只应有数据库文件）；
- NFS 服务端 `nfsstat -c` 不应因查询增加 RPC。

### 16.2 扫描资源

另开终端：

```bash
pidstat -p "$(pgrep -f 'dirspace daemon')" 5 12
systemctl show dirspace -p CPUUsageNSec -p MemoryCurrent -p TasksCurrent
cat /sys/fs/cgroup/system.slice/dirspace.service/cpu.stat
cat /sys/fs/cgroup/system.slice/dirspace.service/memory.current
nfsstat -c 5
cat /proc/self/mountstats
```

验收：

- cgroup CPU 不超过 `CPUQuota`（10%）；
- `memory.current` 不超过 512M，`memory.events` 无 OOM；
- 应用侧 QPS 不超过 `max_metadata_qps`（可加调试日志或用 token bucket 计数）；
- NFS 客户端 `nfsstat -c` 的 ops/s 与配置量级一致，未出现重试风暴；
- 业务 Pod 的 P99 延迟没有明显劣化（这是最终标准）。

### 16.3 极端场景

- 100 万/1000 万小文件目录；
- 大量硬链接；
- 稀疏文件；
- 扫描中不断新增/删除文件；
- NFS 超时/ESTALE/服务端重启；
- Ctrl-C、SIGKILL、磁盘满、数据库损坏；
- 两个进程同时扫同一根（应被 flock 拒绝）；
- 两个根同时扫（daemon 串行；手工进程允许，但会叠加 NFS 压力，需自行控制）。

### 16.4 真实 NFS 注意事项

- 先在测试挂载点验证，再上生产；
- 不要在业务高峰做首次全量；
- 确认 NFS 服务端 `nfsstat -s`、网络、存储后端余量；
- 如果 QPS 50 太慢，先小步提高到 100/200，观察服务端和业务延迟，不要一次拉到 1000+；
- 监控客户端 `nfsstat -c` 的 retrans、timeout、authrefresh。

---

## 17. 故障排查

| 现象 | 原因/处理 |
|---|---|
| `路径尚未统计` | 先 `scan --path`；查询不会隐式扫描 |
| `数据库不能位于 NFS 挂载目录` | 把 `database.path` 改到本地盘，如 `/var/lib/dirspace` |
| `数据库不能位于扫描根目录内` | DB 路径和 root 重叠；改 DB 或 root |
| `该扫描根目录已有扫描任务在运行` | 同根 flock 冲突；等待或 `status`，不要强杀正在写的进程 |
| `database is locked` | DB 被其他写事务占用；确认只有单 daemon，`busy_timeout` 足够，DB 在本地盘 |
| `database disk image is malformed` | 停服务，`doctor --repair`；会保留损坏副本并重建空缓存 |
| `permission denied` | systemd 以 root 运行；检查挂载权限、`ProtectHome`、SELinux/AppArmor |
| `扫描很慢` | 默认 50 QPS；看 `status` 进度和 `nfsstat`，不要直接拉高并发 |
| CPU 被限制到很低 | `CPUQuota=10%` 是硬上限；需要更高需改 unit 并评估业务 |
| 内存接近 512M | inventory 是否开启、SQLite 历史是否过大、是否多个进程；`cleanup`/`backup` 后观察 |
| 服务反复重启 | `journalctl -u dirspace -n 200`；常见是配置错误、DB 权限、NFS 根不可用 |
| 查询显示 stale | 超过 `scan.interval`；执行 refresh/scan 或调大 interval |
| 查询显示 partial | 有目录失败或被删除；看 `last_error` 和日志，失败目录保留旧值 |
| 根合计和 `du` 不一致 | 见第 11 节；硬链接归属、目录块、NFS 属性缓存、扫描期间变化 |
| 修改配置不生效 | 严格 YAML 字段；`systemctl restart`；`--config` 路径是否对 |
| `--repair` 后没有数据 | 修复是重建空缓存，必须重新扫描 |

日志：

```bash
journalctl -u dirspace -f
journalctl -u dirspace --since "1 hour ago"
```

---

## 18. 安全与权限

- 扫描只读：`open`/`readdir`/`lstat`，不写业务目录。
- 不跟随符号链接：根最终分量拒绝，目录项跳过；配置路径应使用真实挂载路径，避免中间分量是符号链接。
- 路径必须是绝对路径，拒绝 `..`、NUL 和 `/`。
- 数据库路径拒绝符号链接；DB 文件创建后 `chmod 0600`。
- 锁文件在 `<dbdir>/locks/`，权限 0600，目录 0700。
- systemd `NoNewPrivileges`、`ProtectSystem=strict`、`ProtectHome=read-only`、`PrivateTmp` 等收紧权限。
- 安装脚本以 root 运行；配置 0640；数据目录 0700。
- 卸载默认保留 SQLite 历史；只有显式 `--purge-data` 并确认才删除。
- 不把密钥、token 写入配置；本工具不需要凭据。

---

## 19. 已知限制与后续方向

1. **无实时性**：NFSv3 无变更通知；结果是客户端快照。
2. **无安全增量**：每轮全量；`full_scan_interval` 预留。未来若接入 NFSv4 change attribute 或厂商 API，再实现真正增量。
3. **硬链接归属**：正常全量扫描根合计正确，单目录字节归属可能随扫描顺序变化；跨目录硬链接 + `rebuild`、或失败目录旧行 + 根直属新硬链接，根合计可能多计一次。
4. **挂载边界**：不检测 `xdev`；一级目录下再挂载其他文件系统会继续遍历。若需要“不跨挂载”，需要增加 `st_dev` 边界判断。
5. **属性缓存**：受客户端 NFS attribute cache 影响。
6. **目录块/特殊文件**：与 `du` 可能有小差异；特殊文件只跳过。
7. **内存硬上限**：`MemoryMax` 由内核执行，达到时可能 OOM kill；应用内是提前量。
8. **多根重叠与多节点叠加**：配置允许根重叠；daemon 在单进程内串行，但手工进程可能重复扫同一子树；多个节点各自有独立限速，N 个节点同时扫描会把 NFS 元数据压力叠加到约 N×QPS。建议不要配置嵌套根，并给不同节点/根错开 `startup_delay` 与 `interval`。
9. **大根首次扫描时间**：默认 50 QPS 对千万级文件需要数十小时；这是有意的保护，不是 bug。
10. **未在真实生产 NFS 压测**：上线前必须按第 16 节验证。
11. **消失的一级目录会保留 stale 行**：为避免 NFS 瞬时不可见时误删数据，目录被删除后旧字节仍保留，根会持续 `partial`；目前没有自动清除命令，需要人工维护数据库或等目录重新出现。这是保守策略，不是实时目录清理工具。
12. **阻塞的 NFS 系统调用不可中断**：`lstat`/`readdir`/`open` 卡在 hard mount 时，Go 无法从另一 goroutine 取消该 syscall；`cancel`/SIGTERM 会等内核 NFS 超时返回。生产上请确认网络与挂载参数（如合理的 `timeo`/`retrans`，或维护窗口），不要把本工具当成 NFS 卡死的看门狗。

---

## 20. License

MIT。见 [`LICENSE`](LICENSE)。
