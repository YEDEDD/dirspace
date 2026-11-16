# dirspace 自适应扫描控制逻辑定义（v1 收敛版）

> 日期：2026-11-16  
> 目的：回应专家评审中“控制逻辑未定义”的问题，作为编码前的最后一版逻辑规格  
> 范围：只定义控制逻辑，不改变现有 Scanner / worker pool / SQLite / 调度架构  
> 状态：控制逻辑已评审；P0 部分已实现，P1/P2 未实现（见第 0 节）

---

## 0. 实现状态

### 已实现（P0）

```text
control_mode: observe / safe / auto
control_mode: observe / safe / auto
impact: low / balanced / fast（起步比例和恢复确认次数）
Linux /proc/diskstats 设备采集
await / queue / util 压力判定
设备严重压力时暂停，警告压力时降速
观察期结束后按 worker/p95 估算容量，auto 快速爬坡
启动前设备严重压力准入
auto 连续严重压力回退 safe
observe 保留原有主机/延迟调节，不做设备/worker 动作
worker 单次扫描内只降不升
CPUGuard 改为整机百分比 + 全局 sleep
config explain / monitor 显示模式与影响等级
```

### 尚未实现（P1/P2）

```text
完整的状态机持久化与 status 展示
完整的 1.4 模式切换门槛
多 root 按底层设备共享预算
启动前准入
NFS/FUSE 专用压力指标
HOT/MIXED/IO_BOUND 自动控制
cgroup io.max 集成
```

因此 1.16.0 是“安全保护 + 可观察”的 P0 版本，不是完整自适应版本。

## 0.1 结论

第一版不追求“完整自适应”，只做四件事：

```text
1. 单控制器：QPS 主控，worker 作为并发硬上限
2. 明确状态机：运行、降速、暂停、恢复、排队、取消
3. 明确压力判定：多指标 + 持续时间 + 滞回 + 冷却
4. 明确 observe / safe / auto 的准入与回退条件
```

第一版不做：

```text
自学习
HOT/MIXED/IO_BOUND 自动控制
worker 在单次扫描内自动恢复
设备 IOPS 精确归因
所有环境强制维护窗口
```

---

## 1. 术语与分层

必须区分三个不同层面，避免把“模式”“扫描状态”“压力状态”混在一起。

### 1.1 控制模式 Mode

```text
observe   只采集、只记录建议，不改变扫描行为
safe      允许自动降速、暂停，不允许自动升速
auto      允许在压力低时自动升速，但硬保护始终有效
```

### 1.2 扫描生命周期 ScanState

```text
Queued       已排队，尚未开始
Starting     正在做准入、基线、初始化
Running      正常扫描
Backoff      压力升高，已降速但仍在扫描
Paused       压力严重或人工暂停，停止取新任务
Recovering   压力恢复中，等待冷却后再恢复
Completed    完成
Failed       失败
Canceled     取消
Deferred     准入未通过，推迟到下一窗口
```

### 1.3 压力状态 PressureState

```text
Normal       所有指标低于警告阈值
Warning      多指标达到警告阈值，并持续确认
Critical     任一关键指标达到严重阈值，或多数指标同时警告
Recovery     压力已回落到恢复阈值以下，等待稳定
```

### 1.4 设备级控制器

```text
一个底层设备一个控制器
同一设备上的多个 root 共享一份 QPS/并发预算
```

不是“一个 root 一个控制器”。

---

## 2. 控制状态机

### 2.1 顶层流程

```text
Queued
  ├─ 准入通过 -> Starting
  └─ 准入不通过 -> Deferred
Starting
  ├─ 初始化成功 -> Running
  └─ 初始化失败 -> Failed
Running
  ├─ Warning -> Backoff
  ├─ Critical -> Paused
  ├─ 完成 -> Completed
  ├─ 取消 -> Canceled
  └─ 错误 -> Failed
Backoff
  ├─ Recovery 且冷却结束 -> Running
  ├─ Critical -> Paused
  └─ 取消 -> Canceled
Paused
  ├─ Recovery 且人工/自动恢复条件满足 -> Recovering
  └─ 取消 -> Canceled
Recovering
  ├─ 稳定 -> Running
  ├─ 再次 Critical -> Paused
  └─ 取消 -> Canceled
```

### 2.2 模式对状态机的限制

| 状态转换 | observe | safe | auto |
|---|---|---|---|
| Running → Backoff | 只记录，不执行 | 允许 | 允许 |
| Running → Paused | 只记录，不执行 | 允许 | 允许 |
| Backoff → Running | 不适用 | 允许，但不升 QPS | 允许，可慢升 |
| Paused → Recovering | 不适用 | 允许 | 允许 |
| Recovering → Running | 不适用 | 允许，回到降速后的值 | 允许 |
| 自动升 QPS | 禁止 | 禁止 | 允许 |

**关键：**

```text
safe 模式可以恢复“继续扫描”，但不会恢复“更高 QPS”
auto 模式才允许升 QPS
```

---

## 3. 压力判定

### 3.1 采样

```text
采样间隔：5 秒
评估间隔：10 秒
决策使用 EWMA + 最近 N 个样本
```

避免单次尖峰直接触发。

### 3.2 指标

本地块设备：

```text
await_ratio      = r_await / baseline_await
queue_ratio      = aqu_sz / target_queue
metadata_p95     = 元数据 p95
cpu_ratio        = dirspace CPU / cpu_budget
io_psi           = cgroup 或主机 IO PSI（可选）
errors           = 超时/错误数
external_busy    = 外部业务负载信号（见 3.4）
```

网络文件系统：

```text
metadata_p95
errors / timeouts
RPC retrans / 吞吐（如可采集）
并发请求数
```

网络盘没有本地 `await / aqu-sz / %util`，不强行伪造。

### 3.3 阈值与滞回

警告阈值必须高于恢复阈值，避免抖动。

| 指标 | 警告 | 严重 | 恢复 |
|---|---:|---:|---:|
| await_ratio | ≥1.5 | ≥2.5 | ≤1.2 |
| queue_ratio | ≥1.5 | ≥3.0 | ≤1.2 |
| metadata_p95 | ≥1.5×target | ≥3×target | ≤1.2×target |
| cpu_ratio | ≥0.8 | ≥1.0 | ≤0.7 |
| errors | >0 | 连续>0 | 0 持续 60s |

**判定规则：**

```text
Warning：
  至少 2 个指标达到警告阈值
  且连续 2 个评估周期（约 20 秒）

Critical：
  任一关键指标达到严重阈值，连续 2 个周期
  或 3 个指标同时达到警告阈值
  或出现连续错误/超时

Recovery：
  所有指标低于恢复阈值
  且持续 6 个评估周期（约 60 秒）
```

### 3.4 `%util` 的定位

```text
HDD：只能作为辅助趋势，不作为唯一触发
SSD：不作为控制依据
RAID/LVM：可能失真，不作为唯一依据
NFS/CIFS：没有本地 %util，不使用
```

`%util` 高但 await/queue 正常，不必然暂停；  
`%util` 低但 await/queue 高，仍然要退让。

### 3.5 外部业务负载

第一阶段**不做**：

```text
external IO = 设备总 IOPS - dirspace 自身 IOPS
```

原因：

- QPS 不等于 IOPS
- 元数据可能命中缓存
- 同一设备上有多个业务进程
- `/proc/diskstats` 是设备级统计

第一阶段只做保守判断：

```text
设备压力高
且 dirspace 实际 QPS 明显低于目标
    -> 可能存在外部负载
    -> 不升速，必要时暂停
```

如果部署了独立 cgroup，可选用：

```text
cgroup v2 io.stat
```

作为增强信号，但不作为第一版必需条件。

---

## 4. QPS 控制规则

### 4.1 启动与探测

第一版不让用户填起始 QPS，也不假设上一轮的安全值。  
启动分两步：

```text
1. 低速率探测 5 秒
2. 根据探测结果决定初始目标
```

探测速率由程序和 worker 上限推导：

```text
probe_qps = clamp(worker_cap × 25, 500, 2000)
```

例如：

```text
worker=20  -> probe_qps = 500
worker=64  -> probe_qps = 1600
```

探测期间采集：

```text
元数据 p95
dirspace CPU/op
设备 await / aqu-sz
外部业务压力
```

探测后估算设备/进程容量：

```text
capacity_est = min(
    worker_cap / p95_metadata,
    cpu_budget / cpu_per_op,
    emergency_cap
)
```

初始目标：

```text
impact low      -> capacity_est × 0.20
impact balanced -> capacity_est × 0.50
impact fast     -> capacity_est × 0.80
```

注意：

```text
capacity_est 只是初始估算，不是最终上限
后续仍由压力反馈决定
```

### 4.1.1 不同机器的起始值示例

起始值不是固定 10,000，而是随 `worker_cap / p95 / cpu_per_op` 变化。

#### 2C4G 小机器示例

```text
effective_cores = 2
cpu_budget = 20% × 2 = 0.4 核
worker_cap = 8（由 CPU/内存推导，不是 64）
probe_qps = clamp(8 × 25, 500, 2000) = 500
p95_metadata = 4ms
cpu_per_op = 100µs

capacity_est = min(
    8 / 0.004 = 2000,
    0.4 / 0.0001 = 4000
) = 2000

impact low      -> 400
impact balanced -> 1000
impact fast     -> 1600
```

不会出现 2C4G 直接 10,000 起步。

#### 40 核 HDD 机器示例

```text
worker_cap = 64
probe_qps = 1600
热缓存 p95 约 1.3ms
capacity_est ≈ 64 / 0.0013 ≈ 49,000
```

对应的起始值才可能到：

```text
low      ≈ 9,800
balanced ≈ 24,500
fast     ≈ 39,200
```

这些数字来自机器能力，不是全局常数。

`safe` 模式允许设定初始目标，但之后不允许动态升 QPS。  
`observe` 模式按现有配置启动，不调整。  
`auto` 模式允许在探测后快速爬坡，再进入慢调。


### 4.2 降速

```text
Warning：
  QPS := max(min_qps, QPS × 0.75)
  进入 Backoff，冷却 60 秒

Critical：
  QPS := max(min_qps, QPS × 0.5)
  进入 Paused 或 Backoff
  冷却 5 分钟

错误/超时：
  立即降 50%
  连续错误则暂停
```

降速不需要等待多个周期，压力确认后立即执行。

### 4.3 升速：两段式

只在 `auto` / `fast` 模式允许。

#### 第一段：探测后的快速爬坡

条件：

```text
PressureState = Normal
设备 await/queue 未达警告
CPU 未达预算
无外部业务压力
```

动作：

```text
QPS := min(target_qps, QPS × 2)
每 5 秒最多一次
最多连续 4 次
```

这条是为了避免热数据扫描在“每 30 秒 +10%”中直接结束。  
按实测，热扫描只有约 80 秒，慢升根本来不及。

#### 第二段：接近目标后的慢调

当 QPS 达到目标的 80% 以上：

```text
PressureState = Recovery
且持续 60 秒
且最近 5 分钟没有降速
动作：QPS := min(hard_cap, QPS × 1.05)
最多每 30 秒一次
```

`safe` 模式：

```text
不动态升 QPS
```

`observe` 模式：

```text
不执行，只记录“本应升/降”
```

### 4.4 硬上限

```text
hard_cap 由程序按资源预算推导，不由用户填固定 QPS
emergency_cap 用于防失控，优先级高于 auto
```

`fast` 只提高预算，不突破：

```text
Critical 暂停
业务外部负载保护
CPU/内存硬限制
容量风险准入
```

---

## 5. worker 控制规则

### 5.1 第一版

```text
worker pool 固定上限
QPS 是主调节量
worker 只降低有效并发，不在单次扫描内自动升高
```

有效并发：

```text
effective_workers = min(configured_workers, controller_limit)
```

压力动作：

```text
Warning：
  effective_workers := max(min_workers, floor(effective_workers × 0.75))

Critical：
  effective_workers := min_workers
  或直接 Paused
```

### 5.2 恢复机制

第一版：

```text
单次扫描内不自动恢复 worker
下一次扫描开始时，重新按配置上限初始化
```

这样避免正反馈，也保留跨扫描的恢复能力。

后续版本可增加：

```text
Recovery 持续 5 分钟
QPS 稳定
设备队列低
每次 +1 worker
最多每 5 分钟一次
且不能与 QPS 升速同时发生
```

但在第一版中不实现。

### 5.3 禁止

```text
禁止：
  worker := 观测延迟 × 目标 QPS 后自动扩张
  延迟越高 → worker 越多
```

因为这会形成：

```text
延迟升高 -> 在途增加 -> worker 增加 -> 队列更长 -> 延迟更高
```

---

## 6. 准入控制

### 6.1 启动前 Admission

检查：

```text
同一设备没有正在运行的高负载扫描
设备压力不在 Critical
外部业务负载不高，或处于低影响窗口
容量风险可接受
当前模式允许启动
```

结果：

```text
通过 -> Queued -> Starting
不通过 -> Deferred，记录原因和下次重试时间
```

### 6.2 运行中检查

准入不是一次性的。

```text
Running 中持续评估
外部业务压力出现 -> Backoff 或 Paused
设备 Critical -> Paused
错误 -> Failed 或 Backoff
```

### 6.3 暂停与恢复

`Paused`：

```text
停止获取新任务
允许在途任务完成
保留断点
不释放扫描锁
```

恢复条件：

```text
PressureState = Recovery
持续 60 秒
冷却期结束
非人工取消
```

恢复动作：

```text
Paused -> Recovering -> Running
恢复后使用降速后的 QPS
不立即回到原 QPS
```

---

## 7. 模式切换门槛

### 7.1 observe → safe

必须满足：

```text
1. 至少完成 3 次完整扫描，或连续观察 7 天
2. 设备指标采集覆盖率 ≥95%
3. 没有持续解析错误
4. 基线有效，且不是业务高负载时采集的
5. 回放/观察记录中，冷数据高压力场景能被识别
6. 运维确认
```

不满足则继续 observe。

### 7.2 safe → auto

必须满足：

```text
1. safe 运行至少 7 天或 3 次完整扫描
2. 没有因误判导致业务事故
3. 没有频繁暂停/恢复（例如 30 分钟内超过 3 次）
4. 降速、暂停、恢复流程验证通过
5. 运维确认
```

### 7.3 auto 自动回退

出现以下任一情况，自动回退到 safe：

```text
连续 3 个采样周期拿不到设备指标
30 分钟内方向切换超过 3 次
Critical 在 10 分钟内出现 2 次
出现无法解释的外部负载且设备压力高
出现连续错误/超时
```

回退后：

```text
不自动回到 auto
需要人工确认后重新开启
```

### 7.4 模式不是性能档位

```text
observe / safe / auto 是“控制权限”
不是“QPS 高低档”
```

`fast` 是 impact 预算，不是绕过保护。

---

## 8. 多 root / 多设备预算

### 8.1 设备分组

```text
按底层块设备 major:minor 或父设备分组
同一设备的 root 共享一个 controller
```

### 8.2 调度规则

```text
同一设备同时只允许一个 exact 扫描处于 Running
其他 exact 进入 Queued
quick 在 exact 运行时默认排队
```

不做：

```text
两个 root 各自跑满 QPS
两个 exact 同时高负载
```

### 8.3 预算分配

```text
设备预算 = controller 当前 QPS/并发上限
active 扫描使用预算
排队扫描不消耗预算
```

如果需要 quick 与 exact 并行，必须显式降低总预算，不能各算一份。

### 8.4 网络盘

```text
按挂载点或服务端分组
没有本地 block device 时，不能按设备预算
默认一次只跑一个扫描
```

---

## 9. CPU 口径

这是 P0，必须先定义。

建议：

```text
effective_cores = min(NumCPU, cgroup CPU quota)
cpu_budget = effective_cores × max_cpu_percent / 100
```

`max_cpu_percent` 表示：

```text
dirspace 进程可使用的主机/ cgroup CPU 总预算百分比
```

不是“每个调用各自 sleep 的比例”。

同时：

```text
CPU 限制必须是全局约束
不能靠每个 worker 自己 sleep 实现
```

如果保留 CPUGuard，应改为全局令牌/信号量，而不是逐调用 sleep。

---

## 10. 容量风险

容量与 QPS 控制分开。

建议分三档，仅作为准入风险信号：

```text
free < 10%     warning
free < 5%      risk
free < 2%      critical
```

同时参考：

```text
inode 使用率
近期增长趋势
业务写入窗口
```

容量风险：

```text
不进入 QPS 公式
不直接断言随机读性能下降
可触发告警、人工确认或推迟 exact
```

beegfs01 当前约 96% 使用、约 1.4 TB 剩余，属于风险信号，不是 QPS 参数。

---

## 11. 可观测性

每次决策必须记录：

```text
时间
设备
模式
ScanState
PressureState
关键指标值
阈值
动作（升/降/暂停/恢复）
原因
```

状态输出示例：

```text
控制模式：safe
扫描状态：Backoff
压力状态：Warning
当前 QPS：3200 / 目标 5000
有效 worker：16 / 20
原因：sdb await 2.1× 基线，queue 2.3× 目标
最近动作：10:42 降速 25%
```

---

## 12. 验收标准

### 12.1 冷数据

```text
设备 await/queue 达警告：
  不再升 QPS
设备达严重：
  降速或暂停
不得出现“磁盘 100% 但 QPS 上限继续上升”
```

### 12.2 业务负载

```text
外部业务使设备高压力：
  dirspace 降速或暂停
业务恢复：
  经过恢复期后可继续
```

### 12.3 热数据

```text
safe：不自动升，保持保守
auto：压力低时逐步升，受 hard_cap 限制
```

### 12.4 稳定性

```text
30 分钟内升降方向切换不超过 3 次
不得出现每秒抖动
暂停/恢复不丢断点
```

### 12.5 多 root

```text
同一设备不会同时跑两个高负载 exact
总预算不被重复计算
```

---

## 13. 第一版范围

### 做

```text
单控制器
QPS 主控
worker 固定上限，只降不升
多指标压力判定
滞回 + 冷却
准入 + 运行时暂停
observe / safe / auto 门槛
多 root 设备预算
CPU 口径修正
```

### 不做

```text
自学习
worker 单次扫描内自动恢复
HOT/MIXED/IO_BOUND 自动控制
设备 IOPS 精确归因
所有环境强制维护窗口
```

---

## 14. 最终原则

```text
QPS 是主调节量
worker 是并发硬上限
压力是多指标综合判断
降速要快，升速要慢
高阈值和恢复阈值必须不同
observe 不改行为
safe 只降不升
auto 才允许升
异常自动回退 safe
同一设备共享一份预算
```

这份定义优先于“更多指标、更多功能”。  
第一版的目标不是聪明，而是**可解释、可保护业务、可验证**。
