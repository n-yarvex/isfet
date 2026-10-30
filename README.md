# ISFET

ISFET 是一个用 Go 写的发送端拥塞控制器，面向基于 UDP 的可靠传输。

排队延迟、丢包和 ECN 先折算成同一个毫秒量纲的信号，窗口和速率两个回路都只看这个信号。
增长很激进，回退有下限，低延迟下的随机丢包有豁免，所以在弱网上不会因为随机丢包就把速率砍半。

状态：实验性。算法只在本仓库的单瓶颈仿真里验证过，没有真实网络的数据，见[已知限制](#已知限制)。

## 特点

- 单信号：丢包、延迟、ECN 融合成一个 `x`，两个回路不会因为信号不同步而互相抵消。
- 窗口 + 速率：窗口限制在途字节，速率决定发送节奏，`PacingRate()` 取两者较小值。
- 增长快：空闲链路上每半个 RTT 就可以把窗口乘 1.75、速率最多乘 5，但速率不会超过实际送达速率的 3 倍。
- 回退有分寸：单次按比例回退 8%–15%，拥塞时再用实际送达速率定锚，避免过冲后收不回来。
- 随机丢包豁免：低排队延迟下，丢包惩罚缩到 10%。
- 保底：有最低速率和最低窗口，反馈中断时分两级衰减，不会直接饿死。
- 无外部依赖，所有公开方法并发安全。

## 安装

```sh
go get github.com/n-yarvex/isfet
```

需要 Go 1.21 或更新版本。把 `OWNER` 换成你自己的 GitHub 用户名或组织名（`go.mod` 和代码里的导入路径也要一起改）。

## 用法

```go
cc := isfet.New(isfet.DefaultConfig())

// 发送：先准入，再按 pacing 间隔写出，最后登记。
if cc.CanSend(n) && time.Now().After(nextSend) {
    conn.Write(pkt)
    cc.OnPacketSent(n)
    gap := float64(n*8) / float64(cc.PacingRate()) // 秒
    nextSend = time.Now().Add(time.Duration(gap * float64(time.Second)))
}

// 收到确认、判丢或放弃时，各调用一次对应的释放方法。
cc.OnPacketAcked(n)   // 或 OnPacketLost(n) / OnPacketAbandoned(n)

// 有新的 RTT 和丢包统计时，每 10–50ms 调用一次。
cc.OnFeedback(isfet.Feedback{
    RTTMs:       rttMs,
    LossShort:   lossShort,   // 最近约 1 RTT 的丢包率
    LossMedium:  lossMedium,  // 最近约 3 RTT 的加权丢包率
    ECNRate:     ecnRate,     // 不支持 ECN 时填 0
    LostPackets: lostPkts,    // 最近 1 RTT 丢的包数
    NowMs:       monotonicMs, // 必须是单调时钟
    HasSample:   true,
})

// 没有新采样时，每 20–50ms 推进一次时间，用来触发超时保护。
cc.OnFeedback(isfet.Feedback{NowMs: monotonicMs})
```

完整的最小示例见 [`example_test.go`](example_test.go)。

## 集成时要注意

- 一条流一个实例。
- `RTTMs` 必须是往返时延。只有单向延迟时乘 2。
- `OnPacketSent` 和三种释放方法必须成对：每个包发送时一次，终结时恰好一次。重传算新的发送。
- `OnPacketAcked` 还被用来估计实际送达速率，不要漏报确认。
- 主机必须真的发数据，并按 `PacingRate()` 做 pacing。发送方长期没有数据时，控制器会停止增长，这是有意的。
- 没有新采样时也要定期调用 `OnFeedback`，否则超时保护不会生效。

接口和所有常量的精确定义在 [`SPEC.md`](SPEC.md)。

## 配置

`DefaultConfig()` 返回推荐值，零值字段会被归一化为默认值。

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `InitialWnd` | 131072 | 初始参考窗口，字节 |
| `InitialRate` | 4000000 | 初始目标速率，bit/s |
| `MinRate` / `MaxRate` | 300000 / 1000000000 | 速率范围，bit/s |
| `MinWnd` / `MaxWnd` | 16384 / 67108864 | 窗口范围，字节 |
| `QdelayTarget` | 50 | 目标排队延迟，毫秒 |
| `MSS` | 1200 | 报文大小，字节 |
| `FeedbackTimeoutMs` | 300 | 反馈超时，毫秒 |

## 仿真结果

仿真是单瓶颈、步长 1 ms 的简化模型，数据由 `go test -run TestSim -v` 生成，细节见 [SPEC §14](SPEC.md#14-仿真数据)。

| 场景 | 利用率 | 平均排队 | 丢包 |
| --- | --- | --- | --- |
| 20 Mbps，RTT 40 ms，缓冲 100 ms | 100.0% | 31.0 ms | 0.04% |
| 同上，3% 随机丢包 | 100.0% | 31.9 ms | 2.99% |
| 同上，8% 随机丢包 | 98.2% | 26.4 ms | 7.99% |
| 20 Mbps，RTT 40 ms，缓冲 10 ms | 100.0% | 8.4 ms | 16.49% |
| 100 Mbps，RTT 100 ms，缓冲 100 ms | 100.0% | 41.8 ms | 0.00% |
| 20 → 5 → 20 Mbps | 99.6% | 50.4 ms | 0.04% |

随机丢包场景的丢包率基本都是注入的随机丢包，拥塞没有额外造成丢包。

## 已知限制

- 缓冲很浅（约 10 ms）的链路上，增长阶段会反复冲到丢包才回退，仿真中丢包约 16%。
- 增长系数高、回退浅，和 Reno、CUBIC、BBR 共享瓶颈时很可能抢到更多带宽，不适合要求公平共存的场景。
- 最小 RTT 的老化很慢，路径 RTT 变长后需要很长时间才能完全跟上。
- 没有真实网络测试，参数也没有针对具体网络调过。

## 测试

```sh
go test ./...
go test -race ./...
go test -run TestSim -v   # 打印仿真数据
```

## 文档

- [`SPEC.md`](SPEC.md)：规范。包含接口、常量、伪代码、不变量、一致性测试和与 1.0 草案的差异。
- 代码与规范是同一套内容。改常量时请同时改两边。

## 许可证

[MIT](LICENSE)
