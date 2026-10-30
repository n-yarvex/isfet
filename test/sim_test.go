package isfet

import (
	"fmt"
	"math/rand"
	"testing"
)

// 简易单瓶颈链路仿真，步长 1ms，只用于回归和文档数据，不是严格的网络模型。

type simCfg struct {
	name      string
	capBps    func(ms int) float64 // 链路容量，bit/s
	baseRTT   int                  // 传播 RTT，ms
	bufferMs  int                  // 瓶颈缓冲，按 5Mbps... 的排空时间折算见 bufBytes
	randLoss  float64              // 入口随机丢包率
	durMs     int
	reportAt  []int
	capSwitch bool
}

type simResult struct {
	utilWindow float64 // 评估区间内平均利用率
	avgQms     float64
	lossPct    float64
	firstHit90 int // 首次达到 90% 容量的时间(ms)，-1 表示未达到
}

func runSim(t *testing.T, sc simCfg, verbose bool) simResult {
	c := New(DefaultConfig())
	rng := rand.New(rand.NewSource(1))
	ring := sc.baseRTT + 2
	ackAt := make([]float64, ring)  // 到点确认的字节
	lostAt := make([]float64, ring) // 到点通知丢失的字节
	sentHist := make([]float64, 4096)
	lostHist := make([]float64, 4096)

	var queue, sentTotal, lostTotal, delivered, qSum float64
	var res simResult
	res.firstHit90 = -1
	frac := 0.0
	evalFrom, evalTo := sc.durMs/3, sc.durMs
	var evalDelivered, evalCap float64

	for ms := 1; ms <= sc.durMs; ms++ {
		capB := sc.capBps(ms) / 8000 // bytes per ms
		bufBytes := capB * float64(sc.bufferMs)
		if sc.capSwitch {
			bufBytes = sc.capBps(1) / 8000 * float64(sc.bufferMs)
		}

		// 发送
		want := float64(c.PacingRate())/8000 + frac
		send := 0.0
		for want >= 1200 && c.CanSend(1200) {
			c.OnPacketSent(1200)
			send += 1200
			want -= 1200
		}
		frac = want
		if frac > 1200 {
			frac = 1200
		}
		sentTotal += send
		sentHist[ms%4096] = send

		lost := 0.0
		if send > 0 && sc.randLoss > 0 {
			lost = send * sc.randLoss
			// 以包为粒度的随机性
			n := int(send / 1200)
			l := 0
			for i := 0; i < n; i++ {
				if rng.Float64() < sc.randLoss {
					l++
				}
			}
			lost = float64(l) * 1200
		}
		queue += send - lost
		if queue > bufBytes {
			lost += queue - bufBytes
			queue = bufBytes
		}
		lostTotal += lost
		lostHist[ms%4096] = lost
		lostAt[(ms+sc.baseRTT)%ring] += lost

		// 排空
		d := capB
		if queue < d {
			d = queue
		}
		queue -= d
		delivered += d
		ackAt[(ms+sc.baseRTT)%ring] += d
		if ms >= evalFrom && ms <= evalTo {
			evalDelivered += d
			evalCap += capB
			qSum += queue / capB
		}

		// 确认和丢失通知，按整包释放
		slot := ms % ring
		for ackAt[slot] >= 1200 {
			c.OnPacketAcked(1200)
			ackAt[slot] -= 1200
		}
		for lostAt[slot] >= 1200 {
			c.OnPacketLost(1200)
			lostAt[slot] -= 1200
		}
		if ackAt[slot] > 0 { // 余数带到下一毫秒
			ackAt[(ms+1)%ring] += ackAt[slot]
			ackAt[slot] = 0
		}
		if lostAt[slot] > 0 {
			lostAt[(ms+1)%ring] += lostAt[slot]
			lostAt[slot] = 0
		}

		if res.firstHit90 < 0 && d >= 0.9*capB && delivered > 0 && float64(c.PacingRate()) >= 0.9*sc.capBps(ms) {
			res.firstHit90 = ms
		}

		// 每 10ms 一次反馈
		if ms%10 == 0 {
			rtt := float64(sc.baseRTT) + queue/capB
			rttI := int(rtt + 0.5)
			sumWin := func(h []float64, w int) float64 {
				s := 0.0
				for i := 0; i < w && i < ms; i++ {
					s += h[(ms-i+4096*2)%4096]
				}
				return s
			}
			w1, w3 := rttI, 3*rttI
			if w3 > 4000 {
				w3 = 4000
			}
			s1, l1 := sumWin(sentHist, w1), sumWin(lostHist, w1)
			s3, l3 := sumWin(sentHist, w3), sumWin(lostHist, w3)
			var short, med float64
			if s1 > 0 {
				short = l1 / s1
			}
			if s3 > 0 {
				med = l3 / s3
			}
			c.OnFeedback(Feedback{RTTMs: uint32(rttI), LossShort: short, LossMedium: med,
				LostPackets: uint32(l1 / 1200), NowMs: int64(ms), HasSample: true})
		}

		if verbose {
			for _, r := range sc.reportAt {
				if ms == r {
					fmt.Printf("  t=%5dms wnd=%9d target=%10d pacing=%10d inflight=%9d queue=%5.1fms\n",
						ms, c.Window(), c.TargetRate(), c.PacingRate(), c.BytesInFlight(), queue/capB)
				}
			}
		}
	}
	res.utilWindow = evalDelivered / evalCap
	res.avgQms = qSum / float64(evalTo-evalFrom+1)
	res.lossPct = 100 * lostTotal / sentTotal
	checkInvariants(t, c)
	return res
}

func TestSim(t *testing.T) {
	constant := func(bps float64) func(int) float64 { return func(int) float64 { return bps } }
	cases := []simCfg{
		{name: "clean 20Mbps rtt40 buf100ms", capBps: constant(20e6), baseRTT: 40, bufferMs: 100, durMs: 20000,
			reportAt: []int{0, 100, 200, 300, 500, 1000, 2000, 5000}},
		{name: "random loss 3% 20Mbps rtt40", capBps: constant(20e6), baseRTT: 40, bufferMs: 100, randLoss: 0.03, durMs: 20000},
		{name: "random loss 8% 20Mbps rtt40", capBps: constant(20e6), baseRTT: 40, bufferMs: 100, randLoss: 0.08, durMs: 20000},
		{name: "shallow buffer 20Mbps rtt40 buf10ms", capBps: constant(20e6), baseRTT: 40, bufferMs: 10, durMs: 20000},
		{name: "100Mbps rtt100 buf100ms", capBps: constant(100e6), baseRTT: 100, bufferMs: 100, durMs: 30000},
	}
	for _, sc := range cases {
		fmt.Println(sc.name)
		r := runSim(t, sc, len(sc.reportAt) > 0)
		fmt.Printf("  => util=%.1f%% avgQueue=%.1fms loss=%.2f%% reach90%%=%dms\n", 100*r.utilWindow, r.avgQms, r.lossPct, r.firstHit90)
		if r.utilWindow < 0.5 {
			t.Errorf("%s: utilization %.2f too low", sc.name, r.utilWindow)
		}
	}

	// 带宽突降：20Mbps -> 5Mbps (10s) -> 20Mbps (20s)
	step := func(ms int) float64 {
		if ms > 10000 && ms <= 20000 {
			return 5e6
		}
		return 20e6
	}
	fmt.Println("capacity step 20 -> 5 -> 20 Mbps")
	r := runSim(t, simCfg{name: "step", capBps: step, baseRTT: 40, bufferMs: 100, durMs: 30000, capSwitch: true,
		reportAt: []int{9000, 10100, 10300, 10600, 11000, 12000, 15000, 20100, 20500, 21000, 22000, 25000}}, true)
	fmt.Printf("  => util=%.1f%% avgQueue=%.1fms loss=%.2f%%\n", 100*r.utilWindow, r.avgQms, r.lossPct)
}
