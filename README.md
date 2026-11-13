# dirspace

`dirspace` 是面向 Linux 的目录空间扫描与缓存服务，适合 NFS、CFS、Lustre 等共享文件系统。它只读元数据，统计扫描根下**一级子目录**的空间、文件数、子目录数、耗时和准确性，并把受限深度的目录索引写入本地 SQLite。日常查询只读本地缓存。

GitHub: <https://github.com/YEDEDD/dirspace>

## 快速开始

需要 Go 1.27+，也可以直接用 Release 中的 Linux 二进制。

```bash
make build-linux
sudo DIRSPACE_BINARY=bin/dirspace-linux-amd64 scripts/install.sh
```

安装脚本会安装二进制、配置、systemd 单元和数据目录，默认启用 `dirspace.service`。已有配置和数据库不会被覆盖。

首次使用：

```bash
dirspace config explain                 # 最终配置、挂载和自动推导值
dirspace quick --path /mnt              # 先出近似结果
dirspace scan --path /mnt               # 再建精确基线
dirspace top 20
dirspace children --path /mnt/cae365
dirspace size --path /mnt/cae365        # 未命中可加 --exact
dirspace status --path /mnt
dirspace monitor --path /mnt
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

NFS 延迟、自动暂停和属性缓存通常比模式差异更大。quick 跑得比 exact 还慢时，先查 NFS，不要加频率。

## 定时与配置

推荐常驻 daemon，它同时维护两套调度：quick 先给近似视图，exact 按周期覆盖。默认 exact 每周、quick 每 24 小时，两套任务串行执行。

最小配置：

```yaml
profile: fast

scan:
  root_path: /mnt
  quick_interval: 24h
  require_mount_point: true
```

其余走默认值：`quick_sample_percent: 2`、`quick_census_files: 16`、`read_batch_size: 1024`、`index_depth: 3`、`index_top_k: 20`、`index_all_dirs: false`、自动内存上限。配置文件里只写要覆盖的项；旧配置中的 `performance` 段仍会覆盖默认值。

生产环境建议保留 `require_mount_point: true`，避免挂载未就绪时误扫本地盘。`root_path` 可以是非根文件系统的挂载点或挂载点下的子目录。

profile 基线：

| profile | QPS 上限 | worker 上限 | CPU |
|---|---:|---:|---:|
| `low` | 100 | 4 | 10% |
| `normal` | 300 | 8 | 20% |
| `fast` | 1000 | 16 | 40% |

`concurrency: auto` 按一级目录数分配 worker，内部由共享信号量限制总并发。QPS 从上限约 1/3 起步，压力正常时逐步升高。`memory_limit: auto` 优先取 cgroup 上限的 80%，否则取物理内存 10%，范围 256MB–4GB。

目录索引用于 `children`：一级目录全部保留，更深层每级只保留 top K。`index_all_dirs: true` 会保存每个目录的聚合，`size` 可查任意深层路径，但数据库会变大。索引不增加 NFS 读取次数，完整扫描成功后原子替换。

## 常用命令

| 命令 | 作用 |
|---|---|
| `dirspace top 20` / `list` | 一级目录排名 / 全部可见目录 |
| `dirspace children --path /mnt/a` | 查看索引中的直接子目录 |
| `dirspace size --path /mnt/a` | 查缓存大小；`--exact` 未命中时精扫该目录 |
| `dirspace fs-usage --path /mnt` | 文件系统容量，不是目录大小 |
| `dirspace status` | 当前任务、进度、ETA、QPS、worker、数据库 |
| `dirspace monitor` | 进程/主机资源和生效配置 |
| `dirspace quick` / `scan` | 快速估算 / 精确全量 |
| `dirspace scan --new` | 只扫新增或失效的一级目录 |
| `dirspace partial` | 扫描中查看本轮与上次结果的合并视图 |
| `dirspace merge --inputs a.db,b.db` | 合并分片数据库 |
| `dirspace config explain` | 最终生效配置和自动推导值 |
| `dirspace daemon` | 常驻调度 quick + exact |

`pause`、`resume`、`cancel`、`refresh`、`rebuild`、`cleanup`、`backup`、`doctor` 等完整命令见 `dirspace --help`。常用参数：`--path`、`--config`、`--db`、`--json`、`--logical`、`--foreground`、`--background`。

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
- exact 每轮遍历全部元数据，没有可靠的 NFS/Lustre 增量；`scan --new` 只看新增或非 fresh 的一级目录，不检查已有目录内部变化。
- quick 的文件数按枚举计数，字节是抽样估算，没有置信区间；小目录通常很准，大目录遇到超大文件漏采会偏差。
- 硬链接在 exact 根内只计一次，归属目录可能因并发而变化；跨分片硬链接不去重。
- 共享文件系统客户端有属性缓存，结果不是实时值；删除的目录可能短暂显示 `stale`，随后隐藏并清理。
- `partial` 是合并视图，不是同一时点的完整快照，不能用于计费或严格趋势对比。
- 精确、要求低延迟的 500T 级根不适合单机全量，应使用分片或多客户端方案。

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
