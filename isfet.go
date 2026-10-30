// Package isfet 实现 ISFET 发送端拥塞控制器。
//
// 丢包、ECN 和排队延迟被折算成同一个毫秒量纲的信号 x，
// 窗口回路和速率回路都只看 x。算法细节见仓库中的 SPEC.md。
package isfet

import (
	"math"
	"sync"
	"time"
)

// 控制系数。数值与 SPEC.md 第 3 节一一对应。
const (
	rateGrowBelow = 0.5 // x < target*0.5 时允许速率增长
	congestAt     = 0.9 // x >= target*0.9 判定为拥塞

	wndGrowNum     uint64 = 7 // 窗口增长: w*7/4 + 8*MSS
	wndGrowDen     uint64 = 4
	wndGrowMSS     uint64 = 8
	wndInflightCap uint64 = 3 // 增长后的窗口不超过 3 倍在途字节
	utilFloorDiv   uint64 = 4 // 在途不足可用量的 1/4 视为应用受限

	wndKeepSevere uint64 = 85 // 窗口回退后保留百分比
	wndKeepNormal uint64 = 92
	pctBase       uint64 = 100

	sustainedCooldownMul int64 = 3 // 持续拥塞时窗口回退间隔: 3*srtt

	rateFloorSevere = 0.85 // 速率单次回退的保留比下限
	rateFloorNormal = 0.90
	rateSlopeSevere = 0.15
	rateSlopeNormal = 0.08

	gammaMax  = 4.0 // 速率增长: r*(1+gamma)
	gammaNum  = 80.0
	gammaBias = 20.0

	// 确认速率（实际送达速率）用来约束增长幅度，并在拥塞时给回退定锚。
	rateGrowCapMul  uint64 = 3 // 增长后的速率不超过 3 倍确认速率
	rateDropCapNum  uint64 = 5 // 拥塞回退后的速率不超过 1.25 倍确认速率
	rateDropCapDen  uint64 = 4
	wndBDPQdelayDiv        = 2.0 // 拥塞回退后窗口不超过 ackRate*(minRTT + target/2)
)

// 严重丢包与随机丢包豁免。
const (
	severeBurstMin   uint64 = 8
	severeBurstDiv   uint64 = 5
	severeShortLoss         = 0.12
	severeMediumLoss        = 0.08

	exemptQdelay            = 0.5
	exemptLossEff           = 0.20
	exemptLossMedium        = 0.15
	exemptBurstMin   uint64 = 6
	exemptBurstDiv   uint64 = 8
)

// 信号惩罚。
const (
	maxSignal        = 500.0
	lossPenaltyBase  = 60.0
	lossPenaltyRef   = 0.05
	lossPenaltyExp   = 1.2
	lossExemptScale  = 0.10
	ecnPenaltyBase   = 30.0
	ecnPenaltyRef    = 0.10
	ecnLowDelayScale = 0.25
)

// 超时与准入。
const (
	idleDecayNum  uint64 = 3
	idleDecayDen  uint64 = 4
	idleFloorMul  int64  = 2
	admitExtraDiv uint64 = 4 // 准入上限 = refWnd * 1.25
)

// 默认值与边界。
const (
	defaultMSS               uint32 = 1200
	defaultQdelayTargetMs    uint32 = 50
	defaultFeedbackTimeoutMs uint32 = 300
	defaultMinRate           uint64 = 300000
	absoluteMinWnd           uint64 = 16384
	fallbackMaxWnd           uint64 = 16 * 1024 * 1024
	invalidRangeMul          uint64 = 4
	invalidRateMul           uint64 = 20

	rttUnset      uint32 = math.MaxUint32
	rttMaxMs      uint32 = 60000
	minRTTAgeMs   int64  = 30000
	initialSrttMs uint32 = 50
	srttOldWeight uint64 = 7
	srttDiv       uint64 = 8

	twoPow64 = 18446744073709551616.0
)

// Config 在创建控制器时给出，之后只读。
type Config struct {
	InitialWnd        uint64 // 初始参考窗口，字节
	InitialRate       uint64 // 初始目标速率，bit/s
	MinRate           uint64 // bit/s
	MaxRate           uint64 // bit/s
	QdelayTarget      uint32 // 目标排队延迟，毫秒
	MSS               uint32 // 字节
	MinWnd            uint64 // 字节
	MaxWnd            uint64 // 字节
	FeedbackTimeoutMs uint32 // 反馈超时，毫秒
}

// DefaultConfig 返回推荐配置。
func DefaultConfig() Config {
	return Config{
		InitialWnd:        131072,
		InitialRate:       4000000,
		MinRate:           300000,
		MaxRate:           1000000000,
		QdelayTarget:      50,
		MSS:               1200,
		MinWnd:            16384,
		MaxWnd:            64 * 1024 * 1024,
		FeedbackTimeoutMs: 300,
	}
}

// Feedback 是一次反馈输入。
type Feedback struct {
	RTTMs       uint32  // 本次 RTT 采样，毫秒；既用于平滑 RTT，也用于更新最小 RTT
	LossShort   float64 // 最近约 1 个 RTT 的丢包率，[0,1]
	LossMedium  float64 // 最近约 3 个 RTT 的加权丢包率，[0,1]
	ECNRate     float64 // ECN-CE 标记率，[0,1]；不支持时填 0
	LostPackets uint32  // 最近 1 个 RTT 内丢失的包数
	NowMs       int64   // 单调时钟，毫秒；<=0 时使用内部时钟
	HasSample   bool    // false 表示只推进时间，用于超时检测
}

// Controller 是单条流的拥塞控制器，所有方法并发安全。
type Controller struct {
	mu  sync.Mutex
	cfg Config

	refWnd     uint64
	targetRate uint64

	hasFeedback    bool
	lastFeedbackMs int64
	lastDecayMs    int64
	lastWndGrowMs  int64
	lastWndDropMs  int64
	lastRateGrowMs int64
	lastRateDropMs int64
	minRTTStampMs  int64
	wasCongested   bool

	bytesInFlight uint64
	ackedTotal    uint64 // 累计确认字节
	ackSampleTot  uint64
	ackSampleMs   int64
	ackRate       uint64 // 最近一个采样区间内的确认速率，bit/s，0 表示还没有
	minRTTMs      uint32
	srttMs        uint32
	lossShort     float64
	lossMedium    float64
	ecnRate       float64

	start time.Time
}

type signal struct {
	x      float64 // 毫秒当量
	severe bool
}

// New 按归一化后的配置创建控制器。
func New(cfg Config) *Controller {
	cfg = normalize(cfg)
	return &Controller{
		cfg:        cfg,
		refWnd:     cfg.InitialWnd,
		targetRate: cfg.InitialRate,
		minRTTMs:   rttUnset,
		srttMs:     initialSrttMs,
		start:      time.Now(),
	}
}

func normalize(cfg Config) Config {
	if cfg.MSS == 0 {
		cfg.MSS = defaultMSS
	}
	if cfg.QdelayTarget == 0 {
		cfg.QdelayTarget = defaultQdelayTargetMs
	}
	if cfg.FeedbackTimeoutMs == 0 {
		cfg.FeedbackTimeoutMs = defaultFeedbackTimeoutMs
	}
	if cfg.MinRate == 0 {
		cfg.MinRate = defaultMinRate
	}
	cfg.MinWnd = maxU64(cfg.MinWnd, maxU64(8*uint64(cfg.MSS), absoluteMinWnd))
	if cfg.MaxRate < cfg.MinRate {
		cfg.MaxRate = satMul(cfg.MinRate, invalidRateMul)
	}
	if cfg.MaxWnd == 0 {
		cfg.MaxWnd = maxU64(satMul(cfg.MinWnd, invalidRangeMul), fallbackMaxWnd)
	}
	if cfg.MaxWnd < cfg.MinWnd {
		cfg.MaxWnd = satMul(cfg.MinWnd, invalidRangeMul)
	}
	cfg.InitialRate = clampU64(cfg.InitialRate, cfg.MinRate, cfg.MaxRate)
	cfg.InitialWnd = clampU64(cfg.InitialWnd, cfg.MinWnd, cfg.MaxWnd)
	return cfg
}

// OnFeedback 处理一次反馈。
func (c *Controller) OnFeedback(fb Feedback) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := fb.NowMs
	if now <= 0 {
		now = time.Since(c.start).Milliseconds() + 1
	}
	if !fb.HasSample {
		c.onIdle(now)
		return
	}
	first := !c.hasFeedback
	c.hasFeedback = true
	c.lastFeedbackMs = now

	c.measure(fb, now, first)
	c.sampleAckRate(now)
	s := c.signal(fb.LostPackets)
	c.updateWindow(s, now)
	c.updateRate(s, now)
}

// onIdle 处理没有新采样的调用。空闲超过 T 衰减一次，超过 2T 置底。
func (c *Controller) onIdle(now int64) {
	if c.hasFeedback {
		t := int64(c.cfg.FeedbackTimeoutMs)
		idle := now - c.lastFeedbackMs
		switch {
		case idle > idleFloorMul*t:
			c.targetRate = c.cfg.MinRate
			c.refWnd = c.cfg.MinWnd
		case idle > t && now-c.lastDecayMs >= t:
			c.targetRate = maxU64(c.cfg.MinRate, satMul(c.targetRate, idleDecayNum)/idleDecayDen)
			c.refWnd = maxU64(c.cfg.MinWnd, satMul(c.refWnd, idleDecayNum)/idleDecayDen)
			c.lastDecayMs = now
		}
	}
	c.targetRate = clampU64(c.targetRate, c.cfg.MinRate, c.cfg.MaxRate)
	c.refWnd = clampU64(c.refWnd, c.cfg.MinWnd, c.cfg.MaxWnd)
}

func (c *Controller) measure(fb Feedback, now int64, first bool) {
	rtt := clampU32(fb.RTTMs, 1, rttMaxMs)

	if first || c.srttMs == 0 {
		c.srttMs = rtt
	} else {
		// 加 srttDiv/2 做四舍五入，避免平滑值卡在整数边界上
		c.srttMs = uint32((srttOldWeight*uint64(c.srttMs) + uint64(rtt) + srttDiv/2) / srttDiv)
	}
	c.srttMs = clampU32(c.srttMs, 1, rttMaxMs)

	switch {
	case c.minRTTMs == rttUnset || rtt <= c.minRTTMs:
		c.minRTTMs = rtt
		c.minRTTStampMs = now
	case now-c.minRTTStampMs > minRTTAgeMs:
		// 30 秒内没有再测到最小值，把基准向当前采样抬 1ms
		c.minRTTMs = minU32(c.minRTTMs+1, rtt)
		c.minRTTStampMs = now
	}

	c.lossShort = clamp01(fb.LossShort)
	c.lossMedium = clamp01(fb.LossMedium)
	c.ecnRate = clamp01(fb.ECNRate)
}

// sampleAckRate 每隔至少一个 srtt 更新一次确认速率。
func (c *Controller) sampleAckRate(now int64) {
	if c.ackSampleMs == 0 {
		c.ackSampleMs, c.ackSampleTot = now, c.ackedTotal
		return
	}
	dt := now - c.ackSampleMs
	if dt < int64(c.srttMs) {
		return
	}
	delta := c.ackedTotal - c.ackSampleTot
	c.ackRate = floatToU64(float64(delta) * 8000.0 / float64(dt))
	c.ackSampleMs, c.ackSampleTot = now, c.ackedTotal
}

// windowPkts 估计当前窗口对应的包数，用于突发丢包阈值。
// 应用受限时以实际在途为准，避免窗口虚高导致阈值失效。
func (c *Controller) windowPkts() uint64 {
	basis := c.refWnd
	if c.bytesInFlight > 0 && c.bytesInFlight < basis {
		basis = c.bytesInFlight
	}
	basis = maxU64(basis, c.cfg.MinWnd)
	return maxU64(1, basis/uint64(c.cfg.MSS))
}

func (c *Controller) signal(lost uint32) signal {
	qdelay := int64(c.srttMs) - int64(c.minRTTMs)
	if qdelay < 0 {
		qdelay = 0
	}
	target := float64(c.cfg.QdelayTarget)
	lossEff := math.Max(c.lossShort, c.lossMedium)
	pkts := c.windowPkts()
	lostPkts := uint64(lost)

	lowQ := float64(qdelay) < exemptQdelay*target
	exempt := lowQ &&
		lossEff < exemptLossEff &&
		c.lossMedium < exemptLossMedium &&
		lostPkts <= maxU64(exemptBurstMin, pkts/exemptBurstDiv)

	x := float64(qdelay)
	if lossEff > 0 {
		p := lossPenaltyBase * math.Pow(lossEff/lossPenaltyRef, lossPenaltyExp)
		if exempt {
			p *= lossExemptScale
		}
		x += p
	}
	if c.ecnRate > 0 {
		p := ecnPenaltyBase * (c.ecnRate / ecnPenaltyRef)
		if lowQ {
			p *= ecnLowDelayScale
		}
		x += p
	}
	if math.IsNaN(x) || math.IsInf(x, 0) || x > maxSignal {
		x = maxSignal
	}

	burst := lostPkts >= maxU64(severeBurstMin, pkts/severeBurstDiv)
	severe := !exempt && (burst || c.lossShort >= severeShortLoss || c.lossMedium >= severeMediumLoss)
	return signal{x: x, severe: severe}
}

// underused 在发送方实际用到的量不足控制器允许量的 1/4 时返回 true。
// 此时增长没有意义，窗口和速率都不应该继续膨胀。
func (c *Controller) underused() bool {
	allowed := float64(c.refWnd)
	if r := float64(c.targetRate) * float64(c.srttMs) / 8000.0; r < allowed {
		allowed = r
	}
	return float64(c.bytesInFlight)*float64(utilFloorDiv) < allowed
}

func (c *Controller) updateWindow(s signal, now int64) {
	congested := s.x >= float64(c.cfg.QdelayTarget)*congestAt || s.severe
	half := maxI64(1, int64(c.srttMs)/2)

	if !congested {
		if now-c.lastWndGrowMs > half && !c.underused() {
			grown := satAdd(satMul(c.refWnd, wndGrowNum)/wndGrowDen, wndGrowMSS*uint64(c.cfg.MSS))
			ceiling := maxU64(c.refWnd, satMul(c.bytesInFlight, wndInflightCap))
			c.refWnd = minU64(grown, ceiling)
			c.lastWndGrowMs = now
		}
	} else {
		cooldown := half
		if c.wasCongested {
			cooldown = sustainedCooldownMul * int64(c.srttMs)
		}
		if now-c.lastWndDropMs > cooldown {
			keep := wndKeepNormal
			if s.severe {
				keep = wndKeepSevere
			}
			next := satMul(c.refWnd, keep) / pctBase
			if c.ackRate > 0 && c.minRTTMs != rttUnset {
				bdp := float64(c.ackRate) * (float64(c.minRTTMs) + float64(c.cfg.QdelayTarget)/wndBDPQdelayDiv) / 8000.0
				next = minU64(next, floatToU64(bdp))
			}
			c.refWnd = maxU64(c.cfg.MinWnd, next)
			c.lastWndDropMs = now
		}
	}
	c.wasCongested = congested
	c.refWnd = clampU64(c.refWnd, c.cfg.MinWnd, c.cfg.MaxWnd)
}

func (c *Controller) updateRate(s signal, now int64) {
	target := float64(c.cfg.QdelayTarget)
	growCooldown := maxI64(1, int64(c.srttMs)/2)
	dropCooldown := maxI64(1, int64(c.srttMs))

	switch {
	case s.x < target*rateGrowBelow && !s.severe &&
		now-c.lastRateGrowMs > growCooldown && !c.underused():
		gamma := math.Min(gammaMax, gammaNum/(float64(c.srttMs)+gammaBias))
		next := floatToU64(float64(c.targetRate) * (1 + gamma))
		if c.ackRate > 0 {
			next = minU64(next, maxU64(c.targetRate, satMul(c.ackRate, rateGrowCapMul)))
		}
		c.targetRate = next
		c.lastRateGrowMs = now

	case (s.x >= target*congestAt || s.severe) && now-c.lastRateDropMs > dropCooldown:
		ratio := s.x / target
		var f float64
		if s.severe {
			f = math.Max(rateFloorSevere, 1-rateSlopeSevere*ratio)
		} else {
			f = math.Max(rateFloorNormal, 1-rateSlopeNormal*ratio)
		}
		next := floatToU64(float64(c.targetRate) * f)
		if c.ackRate > 0 {
			next = minU64(next, satMul(c.ackRate, rateDropCapNum)/rateDropCapDen)
		}
		c.targetRate = next
		c.lastRateDropMs = now
	}
	c.targetRate = clampU64(c.targetRate, c.cfg.MinRate, c.cfg.MaxRate)
}

// OnPacketSent 在每个数据包发出时调用一次，重传也算一次新发送。
func (c *Controller) OnPacketSent(bytes uint64) {
	c.mu.Lock()
	c.bytesInFlight = satAdd(c.bytesInFlight, bytes)
	c.mu.Unlock()
}

// OnPacketAcked、OnPacketLost、OnPacketAbandoned 是三种终结状态，
// 每个已发送的包必须且只能触发其中一个。
func (c *Controller) OnPacketAcked(bytes uint64) {
	c.mu.Lock()
	c.ackedTotal = satAdd(c.ackedTotal, bytes)
	c.bytesInFlight = subSat(c.bytesInFlight, bytes)
	c.mu.Unlock()
}
func (c *Controller) OnPacketLost(bytes uint64)      { c.release(bytes) }
func (c *Controller) OnPacketAbandoned(bytes uint64) { c.release(bytes) }

func (c *Controller) release(bytes uint64) {
	c.mu.Lock()
	c.bytesInFlight = subSat(c.bytesInFlight, bytes)
	c.mu.Unlock()
}

func (c *Controller) admitLimit() uint64 {
	return minU64(satAdd(c.refWnd, c.refWnd/admitExtraDiv), c.cfg.MaxWnd)
}

// CanSend 只做窗口准入，发送节奏由调用方按 PacingRate 控制。
func (c *Controller) CanSend(bytes uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	limit := c.admitLimit()
	if c.bytesInFlight >= limit {
		return false
	}
	return bytes <= limit-c.bytesInFlight
}

// PacingRate 返回发送速率，bit/s。取目标速率与准入窗口折算速率中较小者。
func (c *Controller) PacingRate() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	srtt := maxU64(uint64(c.srttMs), 1)
	wndRate := floatToU64(float64(c.admitLimit()) * 8000.0 / float64(srtt))
	return clampU64(minU64(c.targetRate, wndRate), c.cfg.MinRate, c.cfg.MaxRate)
}

// Window 返回参考窗口，字节。
func (c *Controller) Window() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.refWnd
}

// TargetRate 返回速率回路的目标速率，bit/s。
func (c *Controller) TargetRate() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.targetRate
}

// BytesInFlight 返回在途字节。
func (c *Controller) BytesInFlight() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytesInFlight
}

func clampU64(v, lo, hi uint64) uint64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clampU32(v, lo, hi uint32) uint32 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clamp01(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func satAdd(a, b uint64) uint64 {
	if a > math.MaxUint64-b {
		return math.MaxUint64
	}
	return a + b
}

func subSat(a, b uint64) uint64 {
	if a <= b {
		return 0
	}
	return a - b
}

func satMul(a, b uint64) uint64 {
	if a == 0 || b == 0 {
		return 0
	}
	if a > math.MaxUint64/b {
		return math.MaxUint64
	}
	return a * b
}

func floatToU64(v float64) uint64 {
	if math.IsNaN(v) || v <= 0 {
		return 0
	}
	if v >= twoPow64 {
		return math.MaxUint64
	}
	return uint64(v)
}

func maxU64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

func minU64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}

func maxI64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func minU32(a, b uint32) uint32 {
	if a < b {
		return a
	}
	return b
}
