package isfet_test

import (
	"time"

	"github.com/OWNER/isfet"
)

// 发送循环的最小示意：先准入，再按 pacing 间隔写出，最后登记在途字节。
func Example() {
	cc := isfet.New(isfet.DefaultConfig())
	start := time.Now()
	nowMs := func() int64 { return time.Since(start).Milliseconds() + 1 }

	const pktSize = 1200
	var next time.Time

	send := func() {
		if !cc.CanSend(pktSize) || time.Now().Before(next) {
			return
		}
		// conn.Write(pkt)
		cc.OnPacketSent(pktSize)
		gap := float64(pktSize*8) / float64(cc.PacingRate()) // 秒
		next = time.Now().Add(time.Duration(gap * float64(time.Second)))
	}

	onAck := func(rttMs uint32, lossShort, lossMedium float64, lostPkts uint32) {
		cc.OnPacketAcked(pktSize)
		cc.OnFeedback(isfet.Feedback{
			RTTMs:       rttMs,
			LossShort:   lossShort,
			LossMedium:  lossMedium,
			LostPackets: lostPkts,
			NowMs:       nowMs(),
			HasSample:   true,
		})
	}

	// 没有新采样时，每 20–50ms 推进一次时间，用于超时检测。
	tick := func() { cc.OnFeedback(isfet.Feedback{NowMs: nowMs()}) }

	_, _, _ = send, onAck, tick
}
