# ISFET 拥塞控制规范

版本 1.1（草案）。参考实现：[`isfet.go`](isfet.go)。

ISFET（Inflight-Signal Feedback Equilibrium Transport）是发送端拥塞控制器，面向基于 UDP 的可靠传输。
排队延迟、丢包、ECN 在入口被折算成同一个毫秒量纲的信号 `x`，窗口回路和速率回路都只看 `x`。
增长很激进，回退有下限，低延迟下的随机丢包有豁免。

本文中的"必须"、"不得"、"应"按 RFC 2119 的含义使用。
常量表、公式与参考实现冲突时，以参考实现和测试为准，并应当同时修正本文。

## 目录

1. [范围](#1-范围)
2. [术语与单位](#2-术语与单位)
3. [常量](#3-常量)
4. [配置与归一化](#4-配置与归一化)
5. [状态](#5-状态)
6. [接口](#6-接口)
7. [算法](#7-算法)
8. [辅助函数](#8-辅助函数)
9. [不变量](#9-不变量)
10. [主机要求](#10-主机要求)
11. [设计说明](#11-设计说明)
12. [已知限制](#12-已知限制)
13. [一致性测试](#13-一致性测试)
14. [仿真数据](#14-仿真数据)
15. [与 1.0 草案的差异](#15-与-10-草案的差异)

## 1. 范围

控制器只决定两件事：当前允许多少字节在途，以及以多快的速率发送。
可靠性、重传、加密、多流公平性、应用语义（帧边界、可丢弃性）都不在范围内。
一条流使用一个实例。

## 2. 术语与单位

| 名称 | 单位 | 含义 |
| --- | --- | --- |
| `refWnd` | 字节 | 参考窗口。实际准入上限是它的 1.25 倍 |
| `targetRate` | bit/s | 速率回路的目标发送速率 |
| `ackRate` | bit/s | 最近一个采样区间内的确认速率，0 表示还没有 |
| `srttMs` | 毫秒 | RTT 的 7/8 指数平滑值 |
| `minRTTMs` | 毫秒 | 观测到的最小 RTT，带缓慢老化 |
| `x` | 毫秒当量 | 聚合拥塞信号：排队延迟加上丢包和 ECN 的折算惩罚 |
| `severe` | 布尔 | 严重丢包标志 |
| `exempt` | 布尔 | 随机丢包豁免标志 |
| `bytesInFlight` | 字节 | 已发送、尚未确认/判丢/放弃的字节数 |

所有 RTT 字段必须是往返时延。只有单向延迟时，调用方应乘以 2 再填入。
所有时间戳必须来自单调时钟。

## 3. 常量

实现必须使用下表的值。表中名称与 `isfet.go` 中的常量同名。

### 3.1 控制系数

| 名称 | 值 | 说明 |
| --- | --- | --- |
| `rateGrowBelow` | 0.5 | `x < QdelayTarget × 0.5` 时允许速率增长 |
| `congestAt` | 0.9 | `x >= QdelayTarget × 0.9` 判定为拥塞 |
| `wndGrowNum` | 7 | 窗口增长：`refWnd × 7 / 4 + 8 × MSS` |
| `wndGrowDen` | 4 | |
| `wndGrowMSS` | 8 | |
| `wndInflightCap` | 3 | 增长后的窗口不超过 3 倍在途字节（但不会因此缩小） |
| `utilFloorDiv` | 4 | 在途字节不足可用量的 1/4 视为应用受限，暂停增长 |
| `wndKeepSevere` | 85 | 严重拥塞时窗口保留的百分比 |
| `wndKeepNormal` | 92 | 普通拥塞时窗口保留的百分比 |
| `pctBase` | 100 | |
| `sustainedCooldownMul` | 3 | 持续拥塞时，窗口回退的最小间隔为 `3 × srtt` |
| `rateFloorSevere` | 0.85 | 严重拥塞时，速率单次回退的保留比下限 |
| `rateFloorNormal` | 0.90 | 普通拥塞时，速率单次回退的保留比下限 |
| `rateSlopeSevere` | 0.15 | |
| `rateSlopeNormal` | 0.08 | |
| `gammaMax` | 4.0 | 速率增长系数 γ 的上限 |
| `gammaNum` | 80.0 | γ = min(gammaMax, gammaNum / (srtt + gammaBias)) |
| `gammaBias` | 20.0 | |
| `rateGrowCapMul` | 3 | 增长后的速率不超过 3 倍 `ackRate` |
| `rateDropCapNum` | 5 | 拥塞回退后的速率不超过 `ackRate × 5/4` |
| `rateDropCapDen` | 4 | |
| `wndBDPQdelayDiv` | 2.0 | 拥塞回退后窗口不超过 `ackRate × (minRTT + QdelayTarget/2)` |

### 3.2 严重丢包与豁免

| 名称 | 值 | 说明 |
| --- | --- | --- |
| `severeBurstMin` | 8 | 突发丢包下限（包） |
| `severeBurstDiv` | 5 | 突发丢包阈值为 `max(8, 窗口包数 / 5)` |
| `severeShortLoss` | 0.12 | 短期丢包率达到此值为严重 |
| `severeMediumLoss` | 0.08 | 中期丢包率达到此值为严重 |
| `exemptQdelay` | 0.5 | 豁免要求 `qdelay < QdelayTarget × 0.5` |
| `exemptLossEff` | 0.20 | 豁免要求综合丢包率低于此值 |
| `exemptLossMedium` | 0.15 | 豁免要求中期丢包率低于此值 |
| `exemptBurstMin` | 6 | 豁免要求丢包数不超过 `max(6, 窗口包数 / 8)` |
| `exemptBurstDiv` | 8 | |

### 3.3 信号惩罚

| 名称 | 值 | 说明 |
| --- | --- | --- |
| `maxSignal` | 500.0 | `x` 的上限 |
| `lossPenaltyBase` | 60.0 | 丢包惩罚：`60 × (lossEff / 0.05)^1.2` |
| `lossPenaltyRef` | 0.05 | |
| `lossPenaltyExp` | 1.2 | |
| `lossExemptScale` | 0.10 | 豁免时丢包惩罚乘以此值 |
| `ecnPenaltyBase` | 30.0 | ECN 惩罚：`30 × (ecnRate / 0.10)` |
| `ecnPenaltyRef` | 0.10 | |
| `ecnLowDelayScale` | 0.25 | 低排队延迟时 ECN 惩罚乘以此值 |

### 3.4 超时与准入

| 名称 | 值 | 说明 |
| --- | --- | --- |
| `idleDecayNum` | 3 | 空闲超过 T 时，速率和窗口乘以 3/4（每个 T 最多一次） |
| `idleDecayDen` | 4 | |
| `idleFloorMul` | 2 | 空闲超过 2T 时置为下限 |
| `admitExtraDiv` | 4 | 准入上限 = `refWnd + refWnd / 4` |

### 3.5 默认值与边界

| 名称 | 值 | 说明 |
| --- | --- | --- |
| `defaultMSS` | 1200 | |
| `defaultQdelayTargetMs` | 50 | |
| `defaultFeedbackTimeoutMs` | 300 | |
| `defaultMinRate` | 300000 | |
| `absoluteMinWnd` | 16384 | |
| `fallbackMaxWnd` | 16777216 | 仅当配置的 `MaxWnd` 为 0 时使用 |
| `invalidRangeMul` | 4 | |
| `invalidRateMul` | 20 | |
| `rttUnset` | 4294967295 | `minRTTMs` 的未设置标记 |
| `rttMaxMs` | 60000 | |
| `minRTTAgeMs` | 30000 | |
| `initialSrttMs` | 50 | |
| `srttOldWeight` | 7 | |
| `srttDiv` | 8 | |

## 4. 配置与归一化

配置在创建实例时给定，之后只读。

| 字段 | 类型 | 单位 | 推荐值 |
| --- | --- | --- | --- |
| `InitialWnd` | uint64 | 字节 | 131072 |
| `InitialRate` | uint64 | bit/s | 4000000 |
| `MinRate` | uint64 | bit/s | 300000 |
| `MaxRate` | uint64 | bit/s | 1000000000 |
| `QdelayTarget` | uint32 | 毫秒 | 50 |
| `MSS` | uint32 | 字节 | 1200 |
| `MinWnd` | uint64 | 字节 | 16384 |
| `MaxWnd` | uint64 | 字节 | 67108864 |
| `FeedbackTimeoutMs` | uint32 | 毫秒 | 300 |

归一化必须按下列顺序执行：

```text
1.  MSS == 0               → MSS = 1200
2.  QdelayTarget == 0      → QdelayTarget = 50
3.  FeedbackTimeoutMs == 0 → FeedbackTimeoutMs = 300
4.  MinRate == 0           → MinRate = 300000
5.  MinWnd = max(MinWnd, 8 × MSS, 16384)
6.  MaxRate < MinRate      → MaxRate = satMul(MinRate, 20)
7.  MaxWnd == 0            → MaxWnd = max(satMul(MinWnd, 4), 16777216)
8.  MaxWnd < MinWnd        → MaxWnd = satMul(MinWnd, 4)
9.  InitialRate = clamp(InitialRate, MinRate, MaxRate)
10. InitialWnd  = clamp(InitialWnd,  MinWnd,  MaxWnd)
```

第 4 步不可省略：`MinRate` 为 0 会让后面所有以它为下界的夹紧失效，`PacingRate` 可能返回 0。

## 5. 状态

全部状态为实例私有，所有公开方法必须在同一个互斥锁下读写。

| 变量 | 初值 | 含义 |
| --- | --- | --- |
| `refWnd` | `InitialWnd` | 参考窗口 |
| `targetRate` | `InitialRate` | 目标速率 |
| `hasFeedback` | false | 是否收到过带采样的反馈 |
| `lastFeedbackMs` | 0 | 最近一次带采样反馈的时间 |
| `lastDecayMs` | 0 | 最近一次空闲衰减的时间 |
| `lastWndGrowMs` | 0 | 最近一次窗口增长 |
| `lastWndDropMs` | 0 | 最近一次窗口回退 |
| `lastRateGrowMs` | 0 | 最近一次速率增长 |
| `lastRateDropMs` | 0 | 最近一次速率回退 |
| `minRTTStampMs` | 0 | `minRTTMs` 最近一次被刷新或老化的时间 |
| `wasCongested` | false | 上一次反馈是否处于拥塞 |
| `bytesInFlight` | 0 | 在途字节 |
| `ackedTotal` | 0 | 累计确认字节 |
| `ackSampleTot` | 0 | 上次采样 `ackRate` 时的 `ackedTotal` |
| `ackSampleMs` | 0 | 上次采样 `ackRate` 的时间，0 表示尚未开始 |
| `ackRate` | 0 | 确认速率，bit/s |
| `minRTTMs` | `rttUnset` | 最小 RTT |
| `srttMs` | 50 | 平滑 RTT |
| `lossShort`、`lossMedium`、`ecnRate` | 0 | 最近一次反馈的比率 |
| `start` | 创建时刻 | 内部时钟基准，仅在 `NowMs <= 0` 时使用 |

## 6. 接口

### 6.1 反馈输入

| 字段 | 类型 | 范围 | 含义 |
| --- | --- | --- | --- |
| `RTTMs` | uint32 | ≥ 1 | 本次 RTT 采样，同时用于平滑 RTT 和最小 RTT |
| `LossShort` | float64 | [0, 1] | 最近约 1 个 RTT 的丢包率 |
| `LossMedium` | float64 | [0, 1] | 最近约 3 个 RTT 的加权丢包率 |
| `ECNRate` | float64 | [0, 1] | ECN-CE 标记率，不支持时填 0 |
| `LostPackets` | uint32 | ≥ 0 | 最近 1 个 RTT 内丢失的包数 |
| `NowMs` | int64 | 单调 | 毫秒时间戳。`<= 0` 时使用内部时钟 |
| `HasSample` | bool | | false 表示本次只推进时间 |

输入越界时按 §8 的 `clamp01`、`clampU32` 处理，不得报错或 panic。

### 6.2 事件与查询

| 方法 | 作用 |
| --- | --- |
| `OnFeedback(fb)` | 处理一次反馈（§7） |
| `OnPacketSent(n)` | 每个数据包发出时调用一次，重传也算新的发送 |
| `OnPacketAcked(n)` | 终结状态之一：已确认 |
| `OnPacketLost(n)` | 终结状态之一：判丢 |
| `OnPacketAbandoned(n)` | 终结状态之一：放弃重传 |
| `CanSend(n)` | 窗口准入查询 |
| `PacingRate()` | 发送速率，bit/s |
| `Window()`、`TargetRate()`、`BytesInFlight()` | 状态查询 |

每个已发送的包必须恰好触发一次终结事件。重复释放不会让 `bytesInFlight` 下溢，但会让它偏小。

## 7. 算法

### 7.1 OnFeedback

```text
now = NowMs
若 now <= 0：now = 内部时钟毫秒数 + 1

若 HasSample == false：
    执行 §7.2，返回

first = not hasFeedback
hasFeedback = true
lastFeedbackMs = now

§7.3 测量更新
§7.4 确认速率采样
§7.5 计算信号 (x, severe)
§7.6 窗口回路
§7.7 速率回路
```

### 7.2 空闲处理

```text
若 hasFeedback：
    T    = FeedbackTimeoutMs
    idle = now - lastFeedbackMs
    若 idle > 2T：
        targetRate = MinRate
        refWnd     = MinWnd
    否则若 idle > T 且 now - lastDecayMs >= T：
        targetRate = max(MinRate, satMul(targetRate, 3) / 4)
        refWnd     = max(MinWnd,  satMul(refWnd,     3) / 4)
        lastDecayMs = now

targetRate = clamp(targetRate, MinRate, MaxRate)
refWnd     = clamp(refWnd,     MinWnd,  MaxWnd)
```

空闲调用不会触发任何增长。每个 T 内衰减最多一次，因此即使主机每 20 ms 调用一次，也是"T 后衰减一次，2T 后置底"两级。

### 7.3 测量更新

```text
rtt = clampU32(RTTMs, 1, 60000)

若 first 或 srttMs == 0：
    srttMs = rtt
否则：
    srttMs = (7 × srttMs + rtt + 4) / 8        // 四舍五入
srttMs = clampU32(srttMs, 1, 60000)

若 minRTTMs == rttUnset 或 rtt <= minRTTMs：
    minRTTMs = rtt
    minRTTStampMs = now
否则若 now - minRTTStampMs > 30000：
    minRTTMs = min(minRTTMs + 1, rtt)
    minRTTStampMs = now

lossShort  = clamp01(LossShort)
lossMedium = clamp01(LossMedium)
ecnRate    = clamp01(ECNRate)
```

最小 RTT 在 30 秒内没有被再次测到时，才向当前采样抬高 1 ms，之后再等 30 秒。

### 7.4 确认速率采样

`ackedTotal` 由 `OnPacketAcked` 累加。

```text
若 ackSampleMs == 0：
    ackSampleMs = now；ackSampleTot = ackedTotal；返回
dt = now - ackSampleMs
若 dt < srttMs：返回                      // 至少积累一个 srtt
ackRate = floatToU64((ackedTotal - ackSampleTot) × 8000 / dt)
ackSampleMs = now；ackSampleTot = ackedTotal
```

### 7.5 信号

```text
qdelay   = max(0, int64(srttMs) - int64(minRTTMs))     // 必须用有符号数相减
lossEff  = max(lossShort, lossMedium)
pkts     = windowPkts()
lostPkts = LostPackets

lowQ   = qdelay < 0.5 × QdelayTarget
exempt = lowQ
     且 lossEff    < 0.20
     且 lossMedium < 0.15
     且 lostPkts <= max(6, pkts / 8)

x = qdelay
若 lossEff > 0：
    p = 60 × (lossEff / 0.05)^1.2
    若 exempt：p = p × 0.10
    x = x + p
若 ecnRate > 0：
    p = 30 × (ecnRate / 0.10)
    若 lowQ：p = p × 0.25
    x = x + p
若 x 为 NaN、Inf 或 > 500：x = 500

burst  = lostPkts >= max(8, pkts / 5)
severe = (not exempt) 且 (burst 或 lossShort >= 0.12 或 lossMedium >= 0.08)
```

`windowPkts()`：

```text
basis = refWnd
若 0 < bytesInFlight < basis：basis = bytesInFlight     // 应用受限时以实际在途为准
basis = max(basis, MinWnd)
返回 max(1, basis / MSS)
```

### 7.6 窗口回路

```text
congested = (x >= 0.9 × QdelayTarget) 或 severe
half      = max(1, srttMs / 2)

若 not congested：
    若 now - lastWndGrowMs > half 且 not underused()：
        grown   = satAdd(satMul(refWnd, 7) / 4, 8 × MSS)
        ceiling = max(refWnd, satMul(bytesInFlight, 3))
        refWnd  = min(grown, ceiling)
        lastWndGrowMs = now
否则：
    cooldown = wasCongested ? 3 × srttMs : half
    若 now - lastWndDropMs > cooldown：
        keep = severe ? 85 : 92
        next = satMul(refWnd, keep) / 100
        若 ackRate > 0 且 minRTTMs != rttUnset：
            bdp  = ackRate × (minRTTMs + QdelayTarget / 2.0) / 8000
            next = min(next, floatToU64(bdp))
        refWnd = max(MinWnd, next)
        lastWndDropMs = now

wasCongested = congested
refWnd = clamp(refWnd, MinWnd, MaxWnd)
```

`underused()`：

```text
allowed = min(float64(refWnd), float64(targetRate) × srttMs / 8000)
返回 float64(bytesInFlight) × 4 < allowed
```

发送方实际用到的量不足控制器允许量的 1/4 时，增长没有依据，窗口和速率都不应继续膨胀。

### 7.7 速率回路

```text
target       = QdelayTarget
growCooldown = max(1, srttMs / 2)
dropCooldown = max(1, srttMs)

若 x < 0.5 × target 且 not severe 且 now - lastRateGrowMs > growCooldown 且 not underused()：
    gamma = min(4.0, 80.0 / (srttMs + 20.0))
    next  = floatToU64(targetRate × (1 + gamma))
    若 ackRate > 0：
        next = min(next, max(targetRate, satMul(ackRate, 3)))
    targetRate = next
    lastRateGrowMs = now
否则若 (x >= 0.9 × target 或 severe) 且 now - lastRateDropMs > dropCooldown：
    ratio = x / target
    f = severe ? max(0.85, 1 - 0.15 × ratio)
               : max(0.90, 1 - 0.08 × ratio)
    next = floatToU64(targetRate × f)
    若 ackRate > 0：
        next = min(next, satMul(ackRate, 5) / 4)
    targetRate = next
    lastRateDropMs = now

targetRate = clamp(targetRate, MinRate, MaxRate)
```

增长与回退是互斥分支，同一次反馈只会执行其中一个。
`x` 落在 `[0.5, 0.9) × target` 之间时，速率保持不变，窗口仍可增长。

### 7.8 在途字节与查询

```text
OnPacketSent(n):      bytesInFlight = satAdd(bytesInFlight, n)
OnPacketAcked(n):     ackedTotal = satAdd(ackedTotal, n); bytesInFlight = subSat(bytesInFlight, n)
OnPacketLost(n):      bytesInFlight = subSat(bytesInFlight, n)
OnPacketAbandoned(n): bytesInFlight = subSat(bytesInFlight, n)

admitLimit = min(satAdd(refWnd, refWnd / 4), MaxWnd)

CanSend(n):
    若 bytesInFlight >= admitLimit：返回 false
    返回 n <= admitLimit - bytesInFlight

PacingRate():
    wndRate = floatToU64(admitLimit × 8000 / max(srttMs, 1))
    返回 clamp(min(targetRate, wndRate), MinRate, MaxRate)
```

`CanSend` 只做窗口准入。发送节奏由主机按 `PacingRate()` 自行实现。

## 8. 辅助函数

```text
clampU64(v, lo, hi) = v < lo ? lo : (v > hi ? hi : v)      // clampU32 同理

clamp01(v):
    NaN、±Inf、v < 0 → 0
    v > 1            → 1
    否则             → v

satAdd(a, b) = a > MaxUint64 - b ? MaxUint64 : a + b
subSat(a, b) = a <= b ? 0 : a - b
satMul(a, b):
    a == 0 或 b == 0       → 0
    a > MaxUint64 / b      → MaxUint64
    否则                   → a × b

floatToU64(v):
    NaN 或 v <= 0          → 0
    v >= 2^64              → MaxUint64
    否则                   → uint64(v)
```

浮点转整数必须经过 `floatToU64`。NaN、负数、超过 2^64 的值直接转换在 Go 中结果由实现决定。

## 9. 不变量

| 编号 | 内容 |
| --- | --- |
| I1 | `MinWnd ≤ refWnd ≤ MaxWnd` |
| I2 | `MinRate ≤ targetRate ≤ MaxRate` |
| I3 | `MinRate ≤ PacingRate() ≤ MaxRate` |
| I4 | `0 ≤ x ≤ 500` |
| I5 | `lossShort`、`lossMedium`、`ecnRate` 都在 [0, 1] 内且不为 NaN |
| I6 | `1 ≤ srttMs ≤ 60000` |
| I7 | `minRTTMs ∈ [1, 60000]` 或等于 `rttUnset` |
| I8 | `bytesInFlight` 不会因释放而下溢 |
| I9 | `PacingRate() ≤ targetRate` |
| I10 | 在 `PacingRate()` 未被 `MinRate` 抬高时，`PacingRate() ≤ admitLimit × 8000 / srttMs` |

## 10. 主机要求

1. 一条流一个实例。
2. `NowMs` 必须来自单调时钟。主机应始终显式提供，内部时钟只是兜底。
3. 有新采样时以 `HasSample = true` 调用，建议每 10–50 ms 一次。没有新采样时，主机应以 20–50 ms 的周期调用 `HasSample = false`，否则超时保护不会生效。
4. 按 §6.2 对账在途字节。`OnPacketAcked` 同时是确认速率的来源，漏报确认会让 `ackRate` 偏低。
5. 主机必须按 `PacingRate()` 做 pacing，`CanSend` 只做准入。
6. 发送顺序：先 `CanSend` 与 pacing，再写出，最后 `OnPacketSent`。
7. 主机必须真实地发送数据。发送方长期没有数据时，控制器会因应用受限而停止增长（§7.6）。

## 11. 设计说明

这一节只解释各机制的取舍，不重复公式。

**单一信号。** 丢包、延迟、ECN 如果分别驱动不同回路，在时间上不同步时会互相抵消。
这里先把它们都折成毫秒当量再相加，两个回路读同一个 `x` 和同一个 `severe`，方向不会冲突。
丢包惩罚随丢包率的 1.2 次方增长，低丢包率时温和，高丢包率时快速变大。

**窗口与速率分开。** 窗口直接限制在途量，对突发反应快；速率决定发送节奏，变化较平滑。
`PacingRate()` 取两者折算后的较小值，窗口因此是速率的上限。

**窗口回退不会连续贴底。** 拥塞刚出现时窗口按 `srtt / 2` 的冷却回退；
拥塞持续时，回退间隔拉长到 `3 × srtt`，不再每个半 RTT 削一次。
速率回退间隔是 `srtt`，每次最多降到 85%（严重）或 90%（普通）。

**用确认速率给回退定锚。** 延迟信号比实际拥塞晚一个 RTT 以上，而增长又很快，
只靠"每个 RTT 最多降 10–15%"的回退追不上过冲。去掉确认速率这一项后，同一份实现在仿真里
（20 Mbps、RTT 40 ms、缓冲 100 ms）速率冲到容量的 4 倍以上，缓冲区长期打满，平均丢包约 27%。
现在拥塞时速率回退到不超过 `ackRate × 1.25`，窗口回退到不超过对应的 BDP；
增长时速率不超过 `ackRate × 3`。这只用到 `OnPacketAcked`，没有新增主机接口。

**随机丢包豁免。** 低排队延迟、综合丢包率低、中期丢包率低、突发规模小，四个条件必须同时成立。
满足时丢包惩罚乘以 0.10，不是取消。浅缓冲链路拥塞时延迟上升不明显，这四个条件有一部分会被打破，
因此不会被完全豁免，但见 §12。

**最小 RTT 老化。** 路径变长后，永久锁定的最小 RTT 会让 `qdelay` 一直为正。
老化步长很小（30 秒 1 ms），一次抖动不会把基准抬高。代价是路径变化后的跟随很慢，见 §12。

**空闲保护。** 反馈短暂中断（ACK 聚合）只衰减一次，不会置底；超过 2T 才认为链路失效。

**整数饱和与浮点夹紧。** 窗口和速率都是 uint64，乘法和加法必须饱和，减法必须饱和到 0，
浮点结果必须经过 `floatToU64`。这些不是优化，是为了不让异常输入把状态打到回绕值。

## 12. 已知限制

- **浅缓冲链路会反复过冲。** 缓冲只有约 10 ms 时，延迟信号来不及上升，增长阶段会冲到丢包才回退。
  仿真中丢包约 16%（§14）。对回退后短暂禁止增长做过尝试，丢包降到约 11%，
  但启动变慢、随机丢包下利用率变低，因此没有采纳。
- **不是公平性方案。** 与 Reno/CUBIC/BBR 共享瓶颈时，ISFET 以较高的增长系数和较浅的回退抢占带宽，
  很可能占到更大份额。是否可接受取决于部署场景。
- **最小 RTT 老化很慢。** 路径 RTT 变长 50 ms 后，基准需要约 25 分钟才能完全跟上，
  期间 `qdelay` 会被高估。
- **没有真实网络的测量数据。** §14 的数据来自本仓库的单瓶颈仿真，不代表实际网络。
- **确认速率只按一个 srtt 区间采样。** 反馈稀疏或 ACK 严重聚合时，`ackRate` 会很抖。

## 13. 一致性测试

声明符合本规范的实现必须满足 §3 到 §9，并通过下列行为检验。参考实现的对应测试在 `isfet_test.go`。

| 编号 | 场景 | 期望 | 测试 |
| --- | --- | --- | --- |
| T1 | 空闲链路，发送方持续有数据 | 窗口与速率增长到 `MaxWnd`、`MaxRate` | `TestGrowthOnIdleLink` |
| T2 | 突发严重丢包 | 窗口回退一次，冷却内不再回退 | `TestSevereBurstBacksOffOnceThenCooldown` |
| T3 | 低延迟、5% 随机丢包 | 不判严重，`x` 等于豁免后的惩罚 | `TestRandomLossExempt` |
| T4 | 长时间无反馈 | T 后衰减一次，2T 后置底 | `TestIdleTimeoutTwoStages` |
| T5 | 边界输入：RTT=0、丢包=1.0、NaN、Inf、`MaxUint64` 配置 | 不越界，无 NaN | `TestBoundaryInputs` |
| T6 | 持续拥塞 1 秒 | 速率和窗口都不触底 | `TestSustainedCongestionWindowStaysAboveFloorForOneSecond` |
| T7 | `HasSample = false` | 不触发增长，只做空闲处理 | `TestIdleTimeoutTwoStages` |
| T8 | 发送与释放序列 | `bytesInFlight` 不下溢，加法饱和 | `TestInFlightAccounting` |
| T9 | `minRTTMs` 未设置 | `qdelay` 为 0 | `TestMinRTTUnsetGivesZeroQdelay` |
| T10 | 并发调用 | 无数据竞争（`go test -race`） | `TestConcurrent` |
| T11 | 应用受限 | 窗口和速率不膨胀 | `TestNoGrowthWhenAppLimited` |
| T12 | 确认速率 | 增长不超过 3 倍，拥塞回退不超过 1.25 倍 | `TestAckRateAnchorsGrowthAndDrop` |
| T13 | 最小 RTT 老化 | 30 秒后抬高 1 ms，且限频 | `TestMinRTTAging` |

## 14. 仿真数据

由 `sim_test.go` 生成：单瓶颈、步长 1 ms、固定随机种子，反馈周期 10 ms。
运行：`go test -run TestSim -v`。

利用率是评估区间（后 2/3）内送达字节与链路容量之比；"到达 90%"是首次出现实际送达与 pacing 速率都不低于容量 90% 的时刻。

| 场景 | 利用率 | 平均排队 | 丢包 | 到达 90% |
| --- | --- | --- | --- | --- |
| 20 Mbps，RTT 40 ms，缓冲 100 ms | 100.0% | 31.0 ms | 0.04% | 91 ms |
| 同上，3% 随机丢包 | 100.0% | 31.9 ms | 2.99% | 91 ms |
| 同上，8% 随机丢包 | 98.2% | 26.4 ms | 7.99% | 91 ms |
| 20 Mbps，RTT 40 ms，缓冲 10 ms | 100.0% | 8.4 ms | 16.49% | 91 ms |
| 100 Mbps，RTT 100 ms，缓冲 100 ms | 100.0% | 41.8 ms | 0.00% | 721 ms |
| 20 → 5 → 20 Mbps（各 10 秒，缓冲按 20 Mbps 取 100 ms） | 99.6% | 50.4 ms | 0.04% | 不适用 |

随机丢包场景中的丢包率几乎全部来自注入的随机丢包，说明拥塞没有额外造成丢包。

启动轨迹（第一行场景，默认配置）：

| 时间 | `refWnd` | `targetRate` | 排队延迟 |
| --- | --- | --- | --- |
| 100 ms | 140400 | 19.4 Mbps | 0.3 ms |
| 200 ms | 392400 | 58.2 Mbps | 50.3 ms |
| 300 ms | 157560 | 24.2 Mbps | 36.7 ms |
| 500 ms | 508927 | 19.6 Mbps | 29.2 ms |
| 2000 ms | 594000 | 19.9 Mbps | 37.8 ms |
| 5000 ms | 594000 | 19.9 Mbps | 27.7 ms |

200 ms 处是启动过冲：速率冲到约 3 倍容量，随后由确认速率拉回。

带宽突降（10 秒时 20 → 5 Mbps）：

| 时间 | `targetRate` | 排队延迟 |
| --- | --- | --- |
| 10100 ms | 18.2 Mbps | 274.6 ms |
| 11000 ms | 5.6 Mbps | 261.6 ms |
| 12000 ms | 5.1 Mbps | 39.2 ms |

突降后约 2 秒内排队回到目标附近。恢复到 20 Mbps 后，20500 ms 时速率为 20.9 Mbps。

## 15. 与 1.0 草案的差异

实现与规范同步做了下列修改，原因见括号。

- `Feedback` 去掉 `DelayMs`，只保留 `RTTMs`，同时用于平滑 RTT 和最小 RTT（两个几乎相同的 RTT 字段容易填错）。
- 字段重命名：`LossRateShort` → `LossShort`，`PacketsLostLastRTT` → `LostPackets`，`HasNewSample` → `HasSample`。
- 类型 `Isfet` 改为 `Controller`，`Get` 前缀去掉：`GetPacingRate` → `PacingRate`，`GetRefWnd` → `Window`，`GetTargetRate` → `TargetRate`。
- 新增确认速率 `ackRate`，约束增长并给回退定锚（只按固定比例回退时，仿真中过冲严重，见 §11）。
- 新增应用受限判断 `underused()` 与窗口增长上限 `3 × bytesInFlight`（发送方没数据时窗口和速率不应继续膨胀）。
- 持续拥塞时窗口每 `3 × srtt` 允许再回退一次（1.0 只在上升沿回退，窗口停在偏大的位置）。
- 空闲衰减每个 T 最多一次（1.0 每次调用都乘 3/4，调用周期 20 ms 时几次调用就接近置底，两级台阶形同虚设）。
- 用 `hasFeedback` 标志代替 `lastRealFeedbackMs == 0` 判断首个采样（时间戳为 0 时会误判）。
- 最小 RTT 在刷新最小值时同步更新时间戳，只有 30 秒内没有再测到最小值才老化。
- 平滑 RTT 四舍五入，避免整数截断使其无法上升到最近的采样值。
- 突发丢包阈值用 `min(refWnd, bytesInFlight)` 折算包数，应用受限时不再被虚高的窗口放宽。
- 提高激进度：窗口增长 ×1.5 → ×1.75 并加 8×MSS；γ 上限 3 → 4，分子 60 → 80；拥塞线 0.8 → 0.9；回退保留比窗口 80/90 → 85/92，速率 0.80/0.85 → 0.85/0.90；豁免放宽，惩罚缩放 0.15 → 0.10；初始窗口与速率调高。
- 准入倍率保持 1.25。改成 1.5 时，仿真中的平均排队从 31.0 ms 升到 45.8 ms（100 Mbps 场景从 41.8 ms 升到 63.8 ms），吞吐没有变化。
