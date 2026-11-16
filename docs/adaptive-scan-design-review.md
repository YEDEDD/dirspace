# dirspace 自适应扫描设计评审简报

> 日期：2026-11-16  
> 版本：dirspace 1.15.9  
> 实测环境：beegfs01，`/data/beegfs_storage`，XFS on HDD  
> 用途：给存储 / 文件系统 / 性能 / SRE 专家评审，判断设计是否合理、是否通用  
> 状态说明：本文中的“方案”尚未实现；文中所有 QPS、耗时、IO 数据均为实测或明确标注的推算
>
> 评审修订：专家评审后已收敛部分设计，尤其是“设备 IOPS 减法归因”“worker 自动扩张”“impact 百分比”“冷热二元分类”等，请同时阅读 `docs/adaptive-scan-review-response.md`。

---

## 0. 一页结论

### 0.1 已确认的事实

- 同一台机器、同一份数据：
  - 热缓存、64 worker、放开 QPS：**1 分 20 秒完成，平均 49,439 QPS，峰值 71,545 QPS**
  - 冷缓存、64 worker、放开 QPS：**1 分 27 秒只完成 498,480 文件 / 11.23 TB，QPS 约 1.2 万，sdb 已 99%～100%**
- 当前生产配置是 **5000 QPS / 20 worker**：
  - 热数据时明显过慢（被固定上限拖住）
  - 冷数据时仍可能和业务进程 `storage-prod` 争抢同一块 HDD
- 当前自适应控制器只看 CPU、主机 Load、内存、元数据延迟：
  - **不看设备 await / 队列 / 利用率**
  - **不区分“dirspace 自己的 IO”和“业务进程的 IO”**
  - 冷数据测试中，sdb 已经 100%，控制器仍在把 QPS 上限从 33k 逐步抬到 82k
- beegfs01 的 sdb 是 HDD，且 **96% 满**；磁盘空闲时 `storage-prod` 也会把 sdb 打到 94%～99%

### 0.2 核心设计主张

```text
用户只选择“业务影响等级”
程序自己识别：
  存储类型
  冷/热缓存
  自身负载
  外部业务负载
然后动态决定 QPS、worker、是否暂停
```

不采用：

```text
固定 QPS 5000
固定 worker 20
只按 CPU 算 QPS
只按冷数据最坏情况永久限速
```

### 0.3 希望专家重点评审的问题

1. “业务影响预算 + 相对压力”的控制方式，是否能真正保护业务，还是必须依赖 cgroup IO / ionice / 存储侧 QoS？
2. 在设备统计是整块盘、无法直接按进程归因的情况下，如何可靠区分 dirspace 自身 IO 与业务 IO？
3. QPS 主控、worker 从属的两层控制，是否会发生震荡？worker 应该独立闭环还是只按在途请求推导？
4. `%util` 在 HDD、RAID、SSD、NVMe 上分别还能不能用？应以 await、aqu-sz、p99 中的哪些为主？
5. 热/冷检测用“磁盘 IO 次数 / 元数据操作次数”是否足够稳健？
6. 精确全量扫描是否应该强制准入控制，只允许在维护窗口执行？
7. 该方案在 NFS / FUSE / 多挂载点 / 多云主机上是否仍然成立？
8. 96% 满盘的 XFS 是否应直接排除在“性能调优”之外，先做容量治理？

---

## 1. 背景

### 1.1 dirspace 是什么

`dirspace` 是一个 Go + SQLite 的 Linux 目录空间扫描器：

- 后台遍历目录元数据，统计每个目录的 allocated / logical 大小
- 结果写本地 SQLite，日常 `top`、`children`、`size` 查询不访问共享存储
- 支持精确全量、快速抽样、断点续扫、目录索引、daemon 定时调度
- 只读元数据，不读文件内容，不在扫描根写文件

当前部署：

```text
主机：beegfs01
版本：dirspace 1.15.9
扫描根：/data/beegfs_storage
文件系统：XFS，/dev/sdb1
磁盘：29.1 TB HDD，ROTA=1
容量：30 TB 总量，27.92 TB 已用，约 96%，剩余约 1.4 TB
CPU：40 核
内存：125 GB
```

### 1.2 业务约束

最高优先级约束是：

```text
不能影响 beegfs 业务
```

因此不能简单把 QPS 和 worker 拉到最大。  
但“不能影响业务”不等于“永远用 5000 QPS 慢慢跑”，也不等于“热缓存时也要半小时”。

### 1.3 要解决的问题

同一套程序需要部署到不同机器：

- 4 核小机器 / 40 核大机器
- HDD / SATA SSD / NVMe
- 本地 XFS / NFS / FUSE / RAID / LVM
- 空闲机器 / 与业务共享磁盘的机器

不能为每台机器手工调一个 QPS 和 worker。  
需要一套**按环境和当前状态自适应**的通用策略。

### 1.4 适用范围：不是只针对 beegfs01

beegfs01 只是**实测样本**，不是设计目标。  
本文的 QPS、耗时、await 数值只用于说明“同一台机器冷热差异有多大”，不应直接复制到其他机器。

方案面向通用 Linux 环境：

- 本地盘：HDD / SATA SSD / NVMe
- 网络盘：NFS / CIFS / FUSE / 云文件存储
- 块设备：裸盘 / LVM / RAID / 云盘
- 不同规模：4 核小机器 / 40 核以上大机器
- 不同部署：物理机 / 虚拟机 / 容器 / cgroup 限额
- 不同业务：独占盘 / 与数据库、存储服务共享盘

通用的是**控制框架**：

```text
按挂载点和底层设备采集
按存储类型选择控制信号
按业务影响等级给预算
用相对压力做闭环
外部业务忙时暂停
不同机器自学习基线
```

不是通用的：

```text
50,000 QPS
12,000 QPS
64 worker
某个固定 await 毫秒数
```

---

## 2. 当前实现与现状

### 2.1 当前配置

```yaml
profile: auto
scan:
  root_path: /data/beegfs_storage
performance:
  max_metadata_qps: 5000
  concurrency: 20
```

daemon 当前状态：

```text
active
配置：5000 QPS / 20 worker
最近任务：#9 全量（已完成）
耗时：28m16s
平均 QPS：2,343.8 / 限速 5,000
准确性：近似（存在断点续扫）
下次精确扫描：2026-11-23 11:38:49
下次快速扫描：2026-11-17 11:38:49
```

当前磁盘：

```text
sdb 基本空闲，load 0.56
```

### 2.2 当前控制器实际在看什么

当前 `internal/performance` 的控制器主要包含：

```text
元数据 QPS token bucket
进程 CPU 保护
内存限制
宿主机 CPU / Load 检查
慢元数据延迟 / 暂停
错误退避
```

关键缺口：

```text
没有采集 /proc/diskstats
没有按设备计算 await、aqu-sz、%util
没有识别外部业务 IO
没有按 HDD/SSD/NVMe/NFS 分类
没有准入控制
```

因此当前“自适应”只能根据 CPU、内存和延迟调整，无法知道磁盘是否已经被打满。

### 2.3 实测中暴露的控制器行为

冷数据测试时：

```text
sdb %util：99%～100%
r_await：67～87ms
aqu-sz：48～73
QPS 实际：约 1.0 万～1.3 万
```

但日志仍在出现：

```text
资源恢复正常，逐步提高元数据扫描速率
qps=39999.6
qps=47999.5
qps=57599.4
qps=69119.3
qps=82943.2
```

这说明：

```text
控制器把“CPU 没高、延迟 EWMA 没触发”误判为健康
继续提高 QPS 上限
但实际瓶颈已经是 HDD 队列
```

### 2.4 CPU 口径需要专家确认

配置：

```text
max_cpu_percent: 60
```

CLI 文案显示为“CPU：60%（单核）”。  
但热数据实测中，dirspace 进程 `ps %CPU` 达到 **720%～804%**，即约 7.2～8.0 核，扫描仍正常完成。

这与“60% 单核 = 0.6 核”的直觉不一致，可能涉及：

- `max_cpu_percent` 的实际语义
- CPUGuard 是进程总 CPU 比例，还是单核比例
- cgroup / systemd CPUQuota 的实际约束
- `ps %CPU` 的统计口径

这一点需要代码维护者或专家确认；在确认前，不应继续用“60% 单核”去推算 QPS 上限。

---

## 3. 实测数据

### 3.1 热数据：任务 #8

条件：

```text
连续第二轮扫描，元数据基本在 page cache / dentry / inode cache 中
max_metadata_qps: 100000
concurrency: 64
```

结果：

| 指标 | 实测 |
|---|---:|
| 开始 | 2026-11-16 11:03:26 |
| 结束 | 2026-11-16 11:04:46 |
| 耗时 | 1m20.381s |
| 平均 QPS | 49,439.3 |
| 峰值窗口 QPS | 71,545 |
| 自适应上限 | 最高到 82,943 |
| worker | 64 |
| 文件数 | 2,924,716 |
| 空间 | 27.92 TB |
| 进程 CPU | 约 720%～804%（7.2～8.0 核） |
| 主机 Load1 | 峰值约 4.38（40 核） |
| 内存 RSS | 约 200～400 MB |
| sdb | 大部分时间 0 IOPS；单次采样 20 r/s，r_await 14.46ms，%util 6.69% |
| 错误 | 0 |

结论：

```text
热缓存下，这台机器可以跑 5 万 QPS 左右
瓶颈更接近 CPU / worker，而不是 HDD
```

### 3.2 冷数据：任务 #9

条件：

```text
先 drop_caches（pagecache + dentry + inode）
max_metadata_qps: 100000
concurrency: 64
```

冷扫描前 1 分 27 秒采样：

| 时间 | QPS | worker |
|---|---:|---:|
| 11:10:45 | 4,930 | 64 |
| 11:10:53 | 4,964 | 64 |
| 11:11:01 | 6,762 | 64 |
| 11:11:10 | 9,707 | 64 |
| 11:11:18 | 12,518 | 64 |
| 11:11:26 | 12,651 | 64 |
| 11:11:34 | 13,079 | 64 |
| 11:11:42 | 10,125 | 64 |

资源：

```text
sdb r/s：708～836
r_await：67.7～87.0ms
aqu-sz：48～73
%util：99.3%～100%
进程 CPU：26.9%～32.7%（约 0.27～0.33 核）
load1：11.19 → 44.79，top 峰值 51.23
```

进度：

```text
1m27s 时：498,480 文件，11.23 TB
```

随后主动中断，避免继续压业务盘。

之后在 5000 QPS / 20 worker 下继续续扫，最终：

```text
#9 总耗时：28m16s
平均 QPS：2,343.8
准确性：近似（断点续扫）
```

注意：`28m16s` 包含冷缓存、续扫、与业务进程争盘等因素，不能直接当作“纯冷数据一次完整扫描”的基准。

### 3.3 外部业务负载

停止 dirspace 后：

```text
D 状态线程：storage-prod
sdb：r/s 92，r_await 10.96ms，%util 93.9%
storage-prod 读：约 0.64 MB/s
```

停止前：

```text
sdb：r/s 206，%util 98.4%
storage-prod 读：约 1.8～2.2 MB/s
```

结论：

```text
sdb 上的负载不全是 dirspace
即使 dirspace 停止，storage-prod 也可能把 HDD 打到 90% 以上
```

这直接决定了控制器必须识别外部负载。

### 3.4 与早期 5000/20 的对比

任务 #7：

```text
5000 QPS / 20 worker
耗时：13m13s
结果：精确，27.92 TB，2,924,716 文件
```

任务 #8：

```text
热缓存，64 worker，放开 QPS
耗时：1m20s
```

差异：

```text
约 9.9 倍
```

### 3.5 实测的局限

- 热数据是连续第二轮扫描，不代表冷启动
- 冷数据测试中途被中断，完整冷扫描耗时未知
- 只测了 XFS on HDD，没有测 SSD/NVMe/NFS
- 没有在业务高峰做长时间测试
- `storage-prod` 的具体业务含义和 SLO 未确认
- drop_caches 会影响整机缓存，只适合测试，不适合生产

---

## 4. 问题定义

### 4.1 固定 QPS/worker 的问题

```text
热缓存 + 磁盘空闲：5000 太慢，浪费机器能力
冷数据 + HDD：5000 也不一定安全，仍可能和业务争盘
SSD：固定 5000 可能远低于能力
小机器：固定 64 worker 可能过高
NFS：固定本地磁盘策略不适用
```

### 4.2 只用 CPU 推算 QPS 的问题

- CPU 不是冷数据瓶颈：冷扫描 CPU 只有约 0.3 核
- CPU 口径不明确：60% 配置下实测 8 核
- CPU 空闲不代表磁盘空闲
- 多核机器上 CPU 余量很大，但不应该全部给扫描

### 4.3 worker 越多越快的问题

- HDD 随机元数据受寻道限制
- 64 worker 会把 aqu-sz 拉到 48～73
- 排队增加 await，业务延迟变差
- 对 HDD，worker 超过某个点后总吞吐不再增加

### 4.4 共享盘归因问题

设备统计是整个块设备的：

```text
r/s、await、%util 都包含业务进程
```

如果只看设备指标：

```text
可能把业务 IO 误判为 dirspace 的负载
也可能在业务 IO 很高时继续加扫描
```

---

## 5. 建议的通用设计

### 5.1 设计原则

```text
1. 用户不填 QPS / worker
2. 用户只选业务影响等级
3. 程序自学习设备基线和冷热状态
4. 用相对压力控制，不用绝对阈值
5. 业务负载优先，扫描可暂停
6. 快降慢升，升速有预算
7. worker 从属于 QPS 和队列，不单独追高
8. 不同存储类型用不同控制信号
```

### 5.2 用户接口

建议只暴露：

```yaml
performance:
  impact: low        # low / balanced / fast
  allow_pause: true
  schedule:
    exact_window: "02:00-05:00"
```

`impact` 的含义：

| 等级 | 自身 CPU 预算 | 设备占用预算 | 业务忙时 |
|---|---:|---:|---|
| low | ≤15%～20% | ≤20%～25% | 暂停 / 推迟 |
| balanced | ≤30%～40% | ≤30%～40% | 降速 |
| fast | ≤50%～60% | ≤50%～60% | 仅维护窗口 |

这些是预算，不是固定 QPS。  
程序根据当前机器能力把预算翻译成速率。

### 5.3 分层架构

```text
Metrics Collector
  ├─ 自身：QPS、p95、错误、CPU/op、worker、内存
  ├─ 设备：r/s、w/s、r_await、w_await、aqu-sz、%util、read_bytes
  ├─ 主机：CPU、Load、PSI io、cgroup
  └─ 外部：设备总 IOPS - dirspace 自身 IOPS

Storage Classifier
  ├─ HDD / SSD / NVMe
  ├─ NFS / FUSE
  ├─ RAID / LVM
  └─ 基线延迟、队列、可用 CPU

Admission Controller
  ├─ 业务是否忙
  ├─ 是否维护窗口
  ├─ 是否有其他扫描
  └─ 是否允许启动精确扫描

Adaptive Controller
  ├─ QPS 主控
  ├─ worker 从属
  ├─ 冷热切换
  └─ 暂停 / 恢复

Observability
  └─ 当前值、瓶颈、原因、最近调整
```

### 5.4 设备类型识别

不靠型号，靠：

```text
mountinfo：xfs / nfs / fuse
sysfs rotational：0 / 1
queue/nr_requests
低负载基线：await、IOPS
```

分类：

```text
HDD
SATA/SAS SSD
NVMe
NFS
FUSE
RAID/LVM
Unknown
```

### 5.5 压力模型

建议使用归一化压力，而不是绝对 await：

```text
pressure = max(
    p95_metadata / target_p95,
    device_await / baseline_await,
    aqu_sz / target_queue,
    own_cpu / cpu_budget,
    own_device_share / device_budget,
    external_pressure / external_budget
)
```

其中：

```text
baseline_await = 该设备在无 dirspace 负载时的近期基线
target_queue   = 按存储类型设定的队列目标
own_cpu        = dirspace 进程 CPU
own_device_share = dirspace 自身 IOPS / 设备总 IOPS
```

控制规则：

```text
pressure > 1.0       降速
pressure > 1.5       大幅降速 / 暂停
pressure < 0.6       连续健康后升速
外部业务 IO 高       暂停或推迟
错误 / 超时          立即降速
```

### 5.6 QPS 主控

```text
启动：低速探测，避免冷数据一上来打满
热缓存 + 设备空闲：快速升速，但仍受 impact 预算限制
冷数据：按设备压力升速，达到预算即停
业务忙：不升，必要时暂停
```

建议：

```text
升速：每 5～10 秒 +10%～20%，仅在压力低于预算时
降速：立即，-30% 或 -50%
冷却：降速后 15～30 秒内不升；严重异常 5 分钟不升
```

### 5.7 worker 从属

不要把 worker 当成第二个独立积分器。

建议：

```text
worker ≈ 目标在途请求 × 安全系数
```

并受以下约束：

```text
HDD：队列目标低，worker 不宜高
SSD：可以更高
NVMe：可能先撞 CPU
NFS：按服务端延迟
```

如果设备压力高：

```text
先降 QPS 和 worker
不要继续加 worker
```

### 5.8 冷热识别

```text
io_per_metadata = 设备读 IOPS / 元数据 QPS
```

判断：

```text
io_per_metadata 低 → 热缓存，可提高
io_per_metadata 高 → 冷数据，按存储类型限制
```

但这只是启发式，需要专家评审其稳健性。

### 5.9 业务保护

#### 启动前准入

```text
设备基线是否高
业务进程是否在 D 状态
当前是否维护窗口
是否已有扫描
```

不满足则：

```text
推迟精确扫描
只允许 quick
或直接跳过
```

#### 运行中

```text
设备总 IOPS 高、dirspace 自身占比低
    → 业务在忙
    → 暂停或推迟
```

#### 预留余量

```text
HDD：自身占用不超过设备能力的 20%～35%
SSD：按延迟目标，不按 %util
NFS：按 p95 / 错误
```

### 5.10 不同存储类型的策略

| 类型 | 主要瓶颈 | 主要指标 | 建议 |
|---|---|---|---|
| HDD | 随机寻道、队列 | await、aqu-sz、r/s | 低队列，worker 保守，更多并发无益 |
| SATA SSD | 随机 IOPS、延迟 | await、p95、queue | `%util` 不可靠，按延迟 |
| NVMe | CPU、系统调用 | CPU/op、p99 | 高并发，防 CPU 打满 |
| NFS | 服务端延迟 | p95、错误、吞吐 | 不看本地 diskstats |
| FUSE | 单次延迟 | p95、错误 | 低并发 |
| RAID/LVM | 聚合延迟 | await、queue | `%util` 可能失真 |

### 5.11 模式与上线

```text
off       保持现状
observe   只采集、只记录建议，不改 QPS
safe      只降速，不自动升速
auto      完整闭环
```

建议顺序：

```text
1. observe 采集一轮冷/热数据
2. 影子控制器 replay，不实际执行
3. 低影响 canary
4. 维护窗口开启 fast
5. 最后才考虑默认 auto
```

---

## 6. 通用性分析

### 6.1 能通用的部分

- 压力比、基线、预算的控制框架
- 业务负载识别
- 准入 / 暂停
- 快降慢升
- 按设备聚合
- 冷热识别
- 观测与解释

### 6.2 不能直接通用的部分

- 绝对 await / queue 阈值
- 固定 worker 数
- 固定 QPS
- `%util` 在 SSD/RAID 上的解释
- NFS 没有本地 diskstats
- 不同业务的 SLO 未知

### 6.3 仍需专家确认

- 设备级 IO 归因是否足够
- 是否需要 cgroup io.max / ionice / 存储 QoS
- 多租户共享盘是否应该直接禁止精确扫描
- 96% 满盘 XFS 是否应先扩容

### 6.4 多环境部署矩阵

| 环境 | 底层设备识别 | 主要控制信号 | 需要降级/替代 |
|---|---|---|---|
| 本地 HDD | `/proc/diskstats`、sysfs | await、aqu-sz、r/s | 不能只加 worker |
| 本地 SATA/SAS SSD | `/proc/diskstats`、sysfs | await 基线、p95、queue | `%util` 不可靠 |
| 本地 NVMe | `/proc/diskstats`、sysfs | CPU/op、p99、queue | 可能先撞 CPU |
| LVM / RAID | 映射到父块设备 | 聚合 await、queue | `%util` 可能失真 |
| NFS / CIFS | mountinfo、RPC 统计 | p95、错误、超时、吞吐 | 没有本地 diskstats，不能用设备 util |
| FUSE | mountinfo、进程延迟 | p95、错误、吞吐 | 低并发，保守 |
| 容器 / cgroup | cgroup quota、进程指标 | CPU quota、内存、IO 压力 | 不能用宿主绝对值 |
| 云网络盘 | 虚拟块设备统计 | 客户端延迟、吞吐、错误 | 设备统计可能不代表服务端 |
| 未知设备 | 仅有挂载点信息 | 元数据 p95、错误、CPU | 保守模式，不自动快跑 |

**多根目录、多挂载点的关键约束：**

```text
一个底层设备一个控制器
同一设备上的多个 root 共享预算
不能两个 root 同时各跑一份高 QPS
调度器必须串行化或聚合配额
```

**不同 CPU/内存规模的约束：**

```text
CPU 预算从 cgroup quota / NumCPU 计算，不写死核数
worker 上限按 CPU、内存和队列推导，不写死 64
小机器默认低并发，大机器也保留业务余量
```

---

## 7. 验收与测试建议

### 7.1 功能验收

```text
热缓存：明显快于 5000 配置
冷数据：不把设备打到持续 100%
业务忙：自动暂停或推迟
业务恢复：自动恢复
无震荡：30 分钟内调整次数有上限
```

### 7.2 安全验收

```text
设备 await 不超过基线 1.5～2 倍
外部业务 IO 出现时，dirspace 自身 IO 明显下降
不出现 OOM
不出现扫描卡死
不出现数据准确性问题
```

### 7.3 测试矩阵

```text
HDD 热 / 冷
SSD 热 / 冷
NVMe 热 / 冷
NFS 高延迟
业务进程同时占盘
扫描中业务突然启动
多挂载点共享同一设备
```

---

## 8. 当前状态与待决策

### 8.1 当前状态

```text
dirspace 1.15.9
daemon active
配置：5000 QPS / 20 worker
最近任务：#9 全量，28m16s，平均 2,343.8 QPS
准确性：近似（断点续扫）
下次精确：2026-11-23 11:38:49
sdb：当前空闲，load 0.56
容量：27.92 TB / 约 30 TB，96%
```

### 8.2 待决策

1. 是否先实现 observe 模式，只采集不改行为？
2. 是否把精确扫描限制在维护窗口？
3. 是否引入 cgroup IO 限制，而不只靠应用层退让？
4. 是否接受“热缓存快、冷数据慢、业务忙暂停”的行为？
5. 96% 满盘是否先做容量治理，再谈性能调优？

---

## 9. 附录：关键原始数据

### 9.1 热数据 #8

```text
mode: full
start: 2026-11-16 11:03:26
end: 2026-11-16 11:04:46
duration: 1m20.381s
avg_qps: 49,439.3
peak_qps_window: 71,545
limit_max: 82,943.2
workers: 64
files: 2,924,716
bytes: 27.92 TB
errors: 0
```

### 9.2 冷数据 #9

```text
mode: full
start: 2026-11-16 11:10:34
interrupted: 2026-11-16 11:12:01
cold_sample:
  qps: 4,930 -> 13,079
  workers: 64
  sdb_r_s: 708-836
  sdb_r_await_ms: 67.7-87.0
  sdb_aqu_sz: 48-73
  sdb_util: 99.3%-100%
  process_cpu: 26.9%-32.7%
  load1: 11.19 -> 44.79
  progress: 498,480 files / 11.23 TB
resumed_at_5000_20:
  completed: 2026-11-16 11:38:49
  total_duration: 28m16s
  avg_qps: 2,343.8
  accuracy: approximate
```

### 9.3 外部业务负载

```text
after dirspace stop:
  D-state: storage-prod
  sdb_r_s: 92
  r_await_ms: 10.96
  util: 93.9%
  storage-prod_read: ~0.64 MB/s
```

---

## 10. 给专家的结论请求

请重点判断：

```text
1. 这个“业务影响预算 + 相对压力 + 存储分类 + 外部负载识别”的框架是否合理
2. 是否有更简单、更可靠的通用控制方法
3. 在无法精确归因设备 IO 的情况下，业务保护是否只能依赖 cgroup/存储侧 QoS
4. worker 是否应该完全由 QPS 推导，还是保留独立慢环
5. 当前 5000/20 的保守配置是否应改为“热快冷慢、业务忙停”
```
