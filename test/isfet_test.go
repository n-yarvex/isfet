package isfet

import (
	"math"
	"sync"
	"testing"
)

// feed 发送一次带采样的反馈。
func feed(c *Controller, now int64, rtt uint32, short, medium, ecn float64, lost uint32) {
	c.OnFeedback(Feedback{RTTMs: rtt, LossShort: short, LossMedium: medium, ECNRate: ecn,
		LostPackets: lost, NowMs: now, HasSample: true})
}

// fill 让在途字节接近准入上限，模拟发送方持续有数据。
func fill(c *Controller) {
	c.mu.Lock()
	c.bytesInFlight = c.admitLimit()
	c.mu.Unlock()
}

func checkInvariants(t *testing.T, c *Controller) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	cfg := c.cfg
	if c.refWnd < cfg.MinWnd || c.refWnd > cfg.MaxWnd {
		t.Fatalf("I1: refWnd=%d out of [%d,%d]", c.refWnd, cfg.MinWnd, cfg.MaxWnd)
	}
	if c.targetRate < cfg.MinRate || c.targetRate > cfg.MaxRate {
		t.Fatalf("I2: targetRate=%d out of range", c.targetRate)
	}
	if c.srttMs < 1 || c.srttMs > rttMaxMs {
		t.Fatalf("I6: srtt=%d", c.srttMs)
	}
	if c.minRTTMs != rttUnset && (c.minRTTMs < 1 || c.minRTTMs > rttMaxMs) {
		t.Fatalf("I7: minRTT=%d", c.minRTTMs)
	}
	for _, v := range []float64{c.lossShort, c.lossMedium, c.ecnRate} {
		if v < 0 || v > 1 || math.IsNaN(v) {
			t.Fatalf("I5: ratio %v", v)
		}
	}
}

func TestNormalize(t *testing.T) {
	cfg := normalize(Config{})
	if cfg.MSS != 1200 || cfg.QdelayTarget != 50 || cfg.FeedbackTimeoutMs != 300 || cfg.MinRate != 300000 {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
	if cfg.MinWnd != 16384 {
		t.Fatalf("MinWnd=%d", cfg.MinWnd)
	}
	if cfg.MaxWnd != 16*1024*1024 {
		t.Fatalf("MaxWnd=%d, want fallback 16MiB", cfg.MaxWnd)
	}
	if cfg.MaxRate != 20*cfg.MinRate {
		t.Fatalf("MaxRate=%d", cfg.MaxRate)
	}
	big := normalize(Config{MSS: 4000, MinWnd: 1, MaxWnd: 10})
	if big.MinWnd != 32000 || big.MaxWnd != 4*32000 {
		t.Fatalf("MinWnd/MaxWnd=%d/%d", big.MinWnd, big.MaxWnd)
	}
	huge := normalize(Config{MinRate: math.MaxUint64 - 1})
	if huge.MaxRate != math.MaxUint64 {
		t.Fatalf("saturating MaxRate expected, got %d", huge.MaxRate)
	}
}

func TestGrowthOnIdleLink(t *testing.T) {
	c := New(DefaultConfig())
	w0, r0 := c.Window(), c.TargetRate()
	now := int64(1000)
	for i := 0; i < 40; i++ {
		fill(c)
		feed(c, now, 40, 0, 0, 0, 0)
		now += 21
		checkInvariants(t, c)
	}
	if c.Window() <= w0*10 || c.TargetRate() <= r0*10 {
		t.Fatalf("expected strong growth, wnd %d->%d rate %d->%d", w0, c.Window(), r0, c.TargetRate())
	}
	for i := 0; i < 200; i++ {
		fill(c)
		feed(c, now, 40, 0, 0, 0, 0)
		now += 21
	}
	if c.Window() != c.cfg.MaxWnd || c.TargetRate() != c.cfg.MaxRate {
		t.Fatalf("should reach caps, got wnd=%d rate=%d", c.Window(), c.TargetRate())
	}
}

func TestNoGrowthWhenAppLimited(t *testing.T) {
	c := New(DefaultConfig())
	w0, r0 := c.Window(), c.TargetRate()
	now := int64(1000)
	for i := 0; i < 50; i++ {
		feed(c, now, 40, 0, 0, 0, 0) // bytesInFlight == 0
		now += 21
	}
	if c.Window() != w0 || c.TargetRate() != r0 {
		t.Fatalf("app-limited sender must not inflate: wnd %d->%d rate %d->%d", w0, c.Window(), r0, c.TargetRate())
	}
}

func TestSevereBurstBacksOffOnceThenCooldown(t *testing.T) {
	c := New(DefaultConfig())
	now := int64(1000)
	for i := 0; i < 10; i++ {
		fill(c)
		feed(c, now, 40, 0, 0, 0, 0)
		now += 21
	}
	w := c.Window()
	fill(c)
	feed(c, now, 40, 0.01, 0.01, 0, 100000) // 突发丢包，远超 pkts/5
	want := w * wndKeepSevere / pctBase
	if got := c.Window(); got != want {
		t.Fatalf("wnd after severe loss = %d, want %d", got, want)
	}
	// 紧接着再来一次：持续拥塞下的窗口回退间隔是 3*srtt，不应再降
	w1 := c.Window()
	feed(c, now+5, 40, 0.01, 0.01, 0, 100000)
	if c.Window() != w1 {
		t.Fatalf("window dropped again inside cooldown: %d -> %d", w1, c.Window())
	}
	checkInvariants(t, c)
}

func TestSustainedCongestionWindowStaysAboveFloorForOneSecond(t *testing.T) {
	c := New(DefaultConfig())
	now := int64(1000)
	for i := 0; i < 20; i++ {
		fill(c)
		feed(c, now, 40, 0, 0, 0, 0)
		now += 21
	}
	// min RTT 已是 40，之后 RTT 抬到 140: qdelay=100ms，持续拥塞 1 秒
	rateBefore := c.TargetRate()
	for i := 0; i < 50; i++ {
		fill(c)
		feed(c, now, 140, 0, 0, 0, 0)
		now += 20
	}
	if c.TargetRate() <= c.cfg.MinRate {
		t.Fatalf("rate hit the floor within 1s of congestion")
	}
	if c.TargetRate() >= rateBefore {
		t.Fatalf("rate should have decreased under congestion")
	}
	if c.Window() <= c.cfg.MinWnd {
		t.Fatalf("window hit the floor within 1s of congestion")
	}
}

func TestRandomLossExempt(t *testing.T) {
	c := New(DefaultConfig())
	now := int64(1000)
	for i := 0; i < 5; i++ {
		fill(c)
		feed(c, now, 40, 0, 0, 0, 0)
		now += 21
	}
	c.mu.Lock()
	c.lossShort, c.lossMedium = 0.05, 0.04
	s := c.signal(2)
	c.mu.Unlock()
	if s.severe {
		t.Fatalf("5%% random loss at low delay must not be severe")
	}
	// 惩罚 60 * 1 * 0.10 = 6
	if math.Abs(s.x-6) > 1e-9 {
		t.Fatalf("x=%v, want 6", s.x)
	}
	// 同样的丢包率，但延迟高: 不豁免
	for i := 0; i < 30; i++ { // 让平滑 RTT 追上 100ms
		now += 21
		feed(c, now, 100, 0.05, 0.04, 0, 2)
	}
	c.mu.Lock()
	s = c.signal(2)
	c.mu.Unlock()
	if s.x < 60 {
		t.Fatalf("exemption must not apply with high queue delay, x=%v", s.x)
	}
}

func TestIdleTimeoutTwoStages(t *testing.T) {
	c := New(DefaultConfig())
	now := int64(1000)
	for i := 0; i < 30; i++ {
		fill(c)
		feed(c, now, 40, 0, 0, 0, 0)
		now += 21
	}
	last := now - 21
	w, r := c.Window(), c.TargetRate()

	// 只推进时间，不能增长
	c.OnFeedback(Feedback{NowMs: last + 100})
	if c.Window() != w || c.TargetRate() != r {
		t.Fatalf("idle call must not change state before timeout")
	}
	// 超过 T：只衰减一次，即使调用很密
	for ts := last + 301; ts <= last+590; ts += 20 {
		c.OnFeedback(Feedback{NowMs: ts})
	}
	if got, want := c.TargetRate(), r*3/4; got != want {
		t.Fatalf("rate after first stage = %d, want %d", got, want)
	}
	if got, want := c.Window(), w*3/4; got != want {
		t.Fatalf("wnd after first stage = %d, want %d", got, want)
	}
	// 超过 2T：置底
	c.OnFeedback(Feedback{NowMs: last + 601})
	if c.TargetRate() != c.cfg.MinRate || c.Window() != c.cfg.MinWnd {
		t.Fatalf("expected floor after 2T")
	}
}

func TestFirstFeedbackAtSmallClock(t *testing.T) {
	c := New(DefaultConfig())
	c.OnFeedback(Feedback{RTTMs: 30, HasSample: true}) // NowMs 缺省，走内部时钟
	if !c.hasFeedback || c.lastFeedbackMs <= 0 {
		t.Fatalf("first sample not recorded")
	}
}

func TestBoundaryInputs(t *testing.T) {
	c := New(DefaultConfig())
	now := int64(1)
	inputs := []Feedback{
		{RTTMs: 0, LossShort: 1, LossMedium: 1, ECNRate: 1, LostPackets: math.MaxUint32, HasSample: true},
		{RTTMs: math.MaxUint32, LossShort: math.NaN(), LossMedium: math.Inf(1), ECNRate: -3, HasSample: true},
		{RTTMs: 1, LossShort: 2, HasSample: true},
	}
	for round := 0; round < 100; round++ {
		for _, fb := range inputs {
			fb.NowMs = now
			now += 7
			c.OnFeedback(fb)
			checkInvariants(t, c)
			if p := c.PacingRate(); p < c.cfg.MinRate || p > c.cfg.MaxRate {
				t.Fatalf("pacing %d out of range", p)
			}
		}
	}
	// 超大配置不应溢出
	huge := New(Config{InitialWnd: math.MaxUint64, InitialRate: math.MaxUint64,
		MaxWnd: math.MaxUint64, MaxRate: math.MaxUint64})
	for i := int64(1); i < 200; i++ {
		huge.mu.Lock()
		huge.bytesInFlight = huge.refWnd
		huge.mu.Unlock()
		feed(huge, i*30, 30, 0, 0, 0, 0)
		checkInvariants(t, huge)
	}
}

func TestMinRTTUnsetGivesZeroQdelay(t *testing.T) {
	c := New(DefaultConfig())
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.signal(0); s.x != 0 || s.severe {
		t.Fatalf("fresh controller: x=%v severe=%v", s.x, s.severe)
	}
}

func TestMinRTTAging(t *testing.T) {
	c := New(DefaultConfig())
	feed(c, 1000, 20, 0, 0, 0, 0)
	feed(c, 2000, 60, 0, 0, 0, 0)
	if c.minRTTMs != 20 {
		t.Fatalf("min=%d", c.minRTTMs)
	}
	feed(c, 40000, 60, 0, 0, 0, 0) // 距上次触底 > 30s
	if c.minRTTMs != 21 {
		t.Fatalf("min after aging=%d, want 21", c.minRTTMs)
	}
	feed(c, 41000, 60, 0, 0, 0, 0)
	if c.minRTTMs != 21 {
		t.Fatalf("aging must be rate limited, min=%d", c.minRTTMs)
	}
}

func TestInFlightAccounting(t *testing.T) {
	c := New(DefaultConfig())
	c.OnPacketSent(1000)
	c.OnPacketSent(500)
	c.OnPacketAcked(1000)
	c.OnPacketLost(500)
	if c.BytesInFlight() != 0 {
		t.Fatalf("inflight=%d", c.BytesInFlight())
	}
	c.OnPacketAbandoned(999) // 重复释放不能下溢
	if c.BytesInFlight() != 0 {
		t.Fatalf("underflow: %d", c.BytesInFlight())
	}
	c.OnPacketSent(math.MaxUint64)
	c.OnPacketSent(10)
	if c.BytesInFlight() != math.MaxUint64 {
		t.Fatalf("expected saturation")
	}
}

func TestCanSendAndPacing(t *testing.T) {
	c := New(DefaultConfig())
	limit := c.Window() + c.Window()/4
	if !c.CanSend(limit) || c.CanSend(limit+1) {
		t.Fatalf("admit limit should be 1.25*refWnd=%d", limit)
	}
	c.OnPacketSent(limit)
	if c.CanSend(1) {
		t.Fatalf("window full but CanSend true")
	}
	p := c.PacingRate()
	if p > c.TargetRate() {
		t.Fatalf("pacing %d > target %d", p, c.TargetRate())
	}
	wndRate := float64(limit) * 8000 / float64(c.srttMs)
	if float64(p) > wndRate+1 && p > c.cfg.MinRate {
		t.Fatalf("pacing %d above window-derived %v", p, wndRate)
	}
}

func TestConcurrent(t *testing.T) {
	c := New(DefaultConfig())
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				switch (i + g) % 5 {
				case 0:
					c.OnPacketSent(1200)
				case 1:
					c.OnPacketAcked(1200)
				case 2:
					c.OnFeedback(Feedback{RTTMs: 40, NowMs: int64(i*10 + 1), HasSample: true})
				case 3:
					_ = c.CanSend(1200)
				default:
					_ = c.PacingRate()
				}
			}
		}(g)
	}
	wg.Wait()
	checkInvariants(t, c)
}

func TestAckRateAnchorsGrowthAndDrop(t *testing.T) {
	c := New(DefaultConfig())
	now := int64(1000)
	// 先让确认速率稳定在约 8 Mbps：每 50ms 确认 50000 字节
	feed(c, now, 50, 0, 0, 0, 0)
	for i := 0; i < 6; i++ {
		now += 50
		c.OnPacketSent(50000)
		c.OnPacketAcked(50000)
		fill(c)
		feed(c, now, 50, 0, 0, 0, 0)
	}
	c.mu.Lock()
	ack := c.ackRate
	target := c.targetRate
	c.mu.Unlock()
	if ack < 7_000_000 || ack > 9_000_000 {
		t.Fatalf("ackRate=%d, want about 8Mbps", ack)
	}
	if target > 3*ack {
		t.Fatalf("growth must stay within 3x ack rate: target=%d ack=%d", target, ack)
	}
	// 拥塞时回退到确认速率附近
	for i := 0; i < 8; i++ {
		now += 50
		c.OnPacketSent(50000)
		c.OnPacketAcked(50000)
		fill(c)
		feed(c, now, 200, 0, 0, 0, 0)
	}
	if got := c.TargetRate(); got > ack*5/4 {
		t.Fatalf("target after congestion=%d, want <= %d", got, ack*5/4)
	}
}
