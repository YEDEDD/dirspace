# dirspace

`dirspace` 是面向 Linux 的大容量目录空间计量与缓存服务，适合 TB～PB 级本地或共享文件系统：XFS、ext4、ZFS、Btrfs、NFS、SMB/CIFS、CephFS、Lustre、GPFS、FUSE 等，只要 Linux 能读出 POSIX 目录树即可。它只读元数据，统计扫描根下**一级子目录**的空间、文件数、子目录数、耗时和准确性，并把目录索引写入本地 SQLite。日常查询只读本地缓存，不绑定某个存储协议。

GitHub: <https://github.com/YEDEDD/dirspace>

## 快速开始

需要 Go 1.27+，也可以直接用 Release 中的 Linux 压缩包。

```bash
tar xzf dirspace-1.15.7-linux-amd64.tar.gz
cd dirspace-1.15.7-linux-amd64
sudo ./scripts/install.sh
```

安装脚本会自动放入：

- 二进制：`/usr/local/bin/dirspace`
- 配置：`/etc/dirspace/config.yaml`
- systemd：`/etc/systemd/system/dirspace.service`
- 数据目录：`/var/lib/dirspace/`

默认 enable 并启动 `dirspace.service`；已有配置和数据库不会被覆盖。仓库内构建也可以用 `make build-linux` 后执行 `sudo scripts/install.sh`。

首次使用：

```bash
dirspace config explain                 # 最终配置、挂载和自动推导值
dirspace quick --path /mnt              # 先出近似结果
dirspace scan --path /mnt               # 再建精确基线
dirspace top 20
dirspace children --path /mnt/cae365 100
dirspace size --path /mnt/cae365        # 未命中可加 --exact
dirspace status --path /mnt
dirspace monitor --path /mnt
dirspace diff --path /mnt --since 7d
```

`quick`、`scan`、`refresh`、`rebuild` 和 `size --exact` 在终端里会自动转后台；需要等结果加 `--foreground`，脚本/CI 可加 `--background`。`--json` 不自动转后台。

## exact 与 quick

| | exact | quick |
|---|---|---|
| 文件元数据 | 每个文件都 `lstat` | 每目录前 16 个 + 剩余文件 2% 抽样 |
| 字节数 | 精确 | 抽样估算 |
| 文件数、目录数 | 精确 | 枚举计数 |
| 准确性标记 | `exact` | `sampled` |
| 默认频率 | 每周 | 每 24 小时 |

两种模式都会遍历所有目录、读取所有目录项，quick **不是只扫 2% 的目录**。区别只在文件 `lstat`：

```text
exact ≈ 文件数 + 目录数 × 2
quick ≈ 目录数 × 2 + 每目录前 16 个文件 + 剩余文件 2%
```

小目录不超过 16 个文件时 quick 基本等于 exact；文件很多、目录很少时 quick 快很多；目录很多、文件很少时两者差不多。quick 的误差取决于文件大小分布，目录里混有超大文件时可能偏差很大，不适合计费。exact 和 quick 都使用共享 worker 池，quick 的抽样文件 `lstat` 还会分批并行；但单个目录的 `READDIR` 仍由一个 worker 完成。

有效速度大致是：

```text
有效 QPS ≈ min(配置 QPS, 并发数 / 平均元数据延迟)
```

慢元数据延迟、自动暂停和属性缓存通常比模式差异更大。quick 跑得比 exact 还慢时，先查存储延迟和元数据服务，不要加频率。

## 定时与配置

推荐常驻 daemon，它同时维护两套调度：quick 先给近似视图，exact 按周期覆盖。默认 exact 每周、quick 每 24 小时，两套任务串行执行。

最小配置：

```yaml
profile: auto

scan:
  root_path: /mnt
  quick_interval: 24h
  require_mount_point: true
```

其余走默认值：`quick_sample_percent: 2`、`quick_census_files: 16`、`read_batch_size: 1024`、`index_all_dirs: true`、自动内存上限。配置文件里只写要覆盖的项；`configs/config.yaml` 保留了所有可选键的注释和默认值，需要时取消注释即可。旧配置中的 `performance` 段仍会覆盖默认值。

生产环境建议保留 `require_mount_point: true`，避免挂载未就绪时误扫本地盘。`root_path` 可以是非根文件系统的挂载点或挂载点下的子目录。

profile 基线：

| profile | QPS 上限 | worker 上限 | CPU |
|---|---:|---:|---:|
| `low` | 100 | 4 | 10% |
| `normal` | 300 | 8 | 20% |
| `fast` | 1000 | 16 | 40% |
| `auto` | 本地 4000 / 共享 1000 / FUSE 300 | 本地 16 / 共享 16 / FUSE 8 | 本 60% / 共 40% / FUSE 20% |

`profile: auto` 按扫描根所在文件系统选择基线：本地盘 QPS 4000、共享盘 QPS 1000、FUSE QPS 300；`performance` 里显式写的键仍然覆盖自动值。已有配置如果写的是 `fast`，安装脚本会保留，不会自动改成 `auto`。

慢元数据暂停阈值使用通用名 `slow_metadata_latency`、`pause_metadata_latency`、`pause_cooldown`；旧配置里的 `nfs_slow_latency`、`nfs_pause_latency`、`nfs_pause_cooldown` 仍然兼容。

`concurrency: auto` 的 worker 池会预留到上限，由共享信号量限制实际总并发。QPS 从上限约 1/3 起步，压力正常时逐步升高。`memory_limit: auto` 优先取 cgroup 上限的 80%，否则取物理内存 10%，范围 256MB–4GB。

### 运行中热更新

扫描进程每秒检查一次配置文件（通常是 `/etc/dirspace/config.yaml`）。保存后无需暂停、重启或续扫，约 1 秒内生效：

- `max_metadata_qps`、`min_metadata_qps`
- `concurrency`：调大立即增加可用并发；调小后新任务等当前元数据调用结束再受限，不中断已发出的 `lstat/readdir`
- `max_cpu_percent`、`max_host_cpu_percent`、`max_load1`
- `slow_metadata_latency`、`pause_metadata_latency`、`pause_cooldown`
- `max_metadata_errors`、`memory_limit`

日志会打印 `检测到配置变更，已热更新运行中的扫描`，`status` 的 QPS 上限也会同步更新。`status` 的当前目录会显示每个一级目录正在处理的并发任务数，例如 `beegfs_meta×8, beegfs_storage×4`；活跃 worker 总数也会单独显示。

以下参数仍只在下次启动/扫描时生效：`root_path`、`size_basis`、`index_*`、`quick_*`、`scan.*_interval`、`startup_delay`。改这些需要重启 daemon 或重新发起扫描。daemon 的调度周期和扫描根在启动时读取；正在运行的任务可以热更新 QPS/CPU/并发，但不会改变已绑定的扫描根。

目录索引用于 `children` 和 `size`。默认 `index_all_dirs: true`，会保存每个目录的聚合，`children` 能列任意目录的直接子目录，`size` 能查任意深层路径；代价是数据库随目录数增长，`children` 输出也可能很多，建议加数量限制，例如 `dirspace children --path /mnt/a 100`。

如果只想保留每层 top K，把 `index_all_dirs` 设为 `false`，再配合 `index_depth` 和 `index_top_k`。索引不增加文件系统读取次数，完整扫描成功后原子替换；修改索引配置需要重新扫描才会填充。

## 常用命令

| 命令 | 作用 |
|---|---|
| `dirspace top 20` / `list` | 一级目录排名 / 全部可见目录 |
| `dirspace children --path /mnt/a [N]` | 查看索引中的直接子目录，N 限制行数 |
| `dirspace size --path /mnt/a` | 查缓存大小；`--exact` 未命中时精扫该目录 |
| `dirspace fs-usage --path /mnt` | 文件系统容量，不是目录大小 |
| `dirspace status` | 当前任务、进度、ETA、QPS、worker、数据库 |
| `dirspace monitor` | 进程/主机资源和生效配置 |
| `dirspace diff --since 7d` | 对比同一模式最近两次完成快照，找增长目录 |
| `dirspace quick` / `scan` | 快速估算 / 精确全量 |
| `dirspace scan --new` | 只扫新增或失效的一级目录 |
| `dirspace partial` | 扫描中查看本轮、进行中和上次结果的合并视图 |
| `dirspace merge --inputs a.db,b.db` | 合并分片数据库 |
| `dirspace config explain` | 最终生效配置和自动推导值 |
| `dirspace daemon` | 常驻调度 quick + exact |

`partial` 在扫描中会列出正在扫描的一级目录，显示它们**截至目前**的空间、文件数和子目录数，并标记 `进行中`；已完成目录和从上次沿用的目录分别汇总。顶部同时显示 `df` 风格的文件系统总量、已用和可用容量。进行中数值每 10 秒左右刷新，下次再运行 `partial` 会继续更新到最新值。`children` 会读取当前任务按 `index_depth`/`index_top_k` 生成的受限实时快照（最多 6 层、最多 10 万行），所以扫描过程中也能下钻已完成的子目录；完整索引仍在扫描结束时写入。

`pause`、`resume`、`cancel`、`refresh`、`rebuild`、`cleanup`、`backup`、`doctor` 等完整命令见 `dirspace --help`。常用参数：`--path`、`--config`、`--db`、`--json`、`--logical`、`--foreground`、`--background`；`diff` 另有 `--since`。

## 增长对比

`diff` 只读本地历史，不重新扫描：

```bash
dirspace diff --path /mnt --since 7d
dirspace diff --path /mnt --since 24h --json
```

它取最近一次完成的 full/quick 快照，再取同一模式在 `--since` 之前最近一次完成快照，按一级目录列出基线和当前值。适合回答“这周哪个项目涨了”。没有同模式基线时会直接报错，不会拿 quick 和 exact 混比；历史受 `retention_days` 限制。

## 后台、暂停与续扫

- 关闭终端或 SSH 断开不会中断后台任务；日志在 `/var/lib/dirspace/manual/`，用 `status` 看进度。
- `systemctl restart` 会中断当前扫描，已完成目录的 staging 保留；下次启动默认续扫，前提是 `scan.resume_interrupted: true`。
- 进程被 `SIGKILL` 后约 90 秒标记中断。exact 和 quick 只恢复各自模式的最新任务，不会把 sampled 结果当 exact 发布。
- 精确扫描进行中，手动 `quick` 会被拒绝，避免覆盖 staging。
- 只替换二进制不影响正在运行的旧进程；要换版本可先 `install`，再在维护窗口 `restart`。

## 大根与分片

单机扫不完时，可以按一级目录分片，多客户端各写本地库，最后合并：

```bash
dirspace --db /var/lib/dirspace/shard-0.db scan --path /mnt --shard 0/4
dirspace --db /var/lib/dirspace/merged.db --path /mnt \
  merge --inputs shard-0.db,shard-1.db,shard-2.db,shard-3.db
```

分片根路径必须一致，分片不能重叠；合并前结果是 `partial`。跨分片硬链接目前不去重，不能宣称精确。分片只能拆一级/子树目录，不能把单个扁平目录拆给多个 worker；多客户端总 QPS 需要人工分配。

## 限制与安全

- 默认按 `allocated`（`st_blocks × 512`）统计，接近 `du`；`--logical` 使用 `st_size`。
- exact 每轮遍历全部元数据，没有可靠的通用增量；`scan --new` 只看新增或非 fresh 的一级目录，不检查已有目录内部变化。
- quick 的文件数按枚举计数，字节是抽样估算，没有置信区间；小目录通常很准，大目录遇到超大文件漏采会偏差。
- 硬链接在 exact 根内只计一次，归属目录可能因并发而变化；跨分片硬链接不去重。
- 共享文件系统客户端有属性缓存，结果不是实时值；删除的目录可能短暂显示 `stale`，随后隐藏并清理。
- `partial` 是合并视图，不是同一时点的完整快照，不能用于计费或严格趋势对比。
- 精确、要求低延迟的 500T 级根不适合单机全量，应使用分片或多客户端方案。
- 成本由文件数、目录数和元数据延迟决定，不是由 TB 数决定；100TB 大文件可能比 10TB 小文件快得多。
- `allocated` 是客户端看到的分配大小；ZFS 压缩/去重、CephFS 稀疏文件、FUSE 对象网关的物理占用可能不同。

只读与冷数据：

- 只用 `Lstat`、`stat`、`readdir`、`fstat`，不 `open` 普通文件，不读内容。
- 不执行 HSM restore，不写扫描目录，不改权限/属主，不更新文件 `atime`。
- SQLite、日志、配置和备份必须放本地盘，不要放共享存储或扫描根内。

## 开发与许可

```bash
make fmt-check
make vet
make test
make test-race
```

性能验证：`bash tests/perf/run.sh`。MIT License，见 [LICENSE](LICENSE)。
