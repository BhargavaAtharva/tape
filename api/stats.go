package api

import (
	"context"
	"sync"
	"time"

	"github.com/BhargavaAtharva/tape/engine"
	"github.com/BhargavaAtharva/tape/feed"
	"github.com/BhargavaAtharva/tape/hist"
)

const statsWindow = time.Second

type latency struct {
	P50  int64 `json:"p50"`
	P99  int64 `json:"p99"`
	P999 int64 `json:"p999"`
}

func percentiles(c *hist.Counts) latency {
	return latency{P50: c.Quantile(0.5), P99: c.Quantile(0.99), P999: c.Quantile(0.999)}
}

type walStats struct {
	Durable       bool   `json:"durable"`
	Written       uint64 `json:"written"`
	Synced        uint64 `json:"synced"`
	Lag           uint64 `json:"lag"`
	SnapshotIndex uint64 `json:"snapshot_index"`
}

type symbolStats struct {
	Symbol       string     `json:"symbol"`
	OrdersPerSec float64    `json:"orders_per_sec"`
	Commands     uint64     `json:"commands"`
	Fills        uint64     `json:"fills"`
	Seq          uint64     `json:"seq"`
	QueueDepth   int        `json:"queue_depth"`
	LatencyNs    latency    `json:"latency_ns"`
	WAL          walStats   `json:"wal"`
	Feed         feed.Stats `json:"feed"`
}

// Stats is the /v1/stats body. Rates and latency percentiles cover the last
// sampling window; counters are totals since start.
type Stats struct {
	At           int64             `json:"at"`
	WindowMs     int64             `json:"window_ms"`
	OrdersPerSec float64           `json:"orders_per_sec"`
	Commands     uint64            `json:"commands"`
	Fills        uint64            `json:"fills"`
	LatencyNs    latency           `json:"latency_ns"`
	Symbols      []symbolStats     `json:"symbols"`
	Info         map[string]string `json:"info,omitempty"`
}

type sampler struct {
	x     *engine.Exchange
	feeds map[string]*feed.Feed

	prev   []engine.Stats
	prevAt time.Time

	mu  sync.Mutex
	cur Stats
}

func newSampler(x *engine.Exchange, feeds map[string]*feed.Feed) *sampler {
	s := &sampler{x: x, feeds: feeds}
	s.sample(time.Now())
	return s
}

func (s *sampler) run(ctx context.Context) {
	t := time.NewTicker(statsWindow)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s.sample(now)
		}
	}
}

func (s *sampler) sample(now time.Time) {
	engines := s.x.Engines()
	cur := make([]engine.Stats, len(engines))
	out := Stats{At: now.UnixMilli(), Symbols: make([]symbolStats, len(engines))}
	elapsed := now.Sub(s.prevAt).Seconds()
	var all hist.Counts
	for i, e := range engines {
		st := e.Stats()
		cur[i] = st
		window := st.Latency
		var rate float64
		if s.prev != nil {
			window = st.Latency.Sub(s.prev[i].Latency)
			rate = float64(st.Commands-s.prev[i].Commands) / elapsed
		}
		all.Add(window)
		out.Symbols[i] = symbolStats{
			Symbol: e.Symbol(), OrdersPerSec: rate, Commands: st.Commands, Fills: st.Fills,
			Seq: st.Seq, QueueDepth: st.QueueDepth, LatencyNs: percentiles(window),
			WAL: walStats{
				Durable: st.Durable, Written: st.WALWritten, Synced: st.WALSynced,
				Lag: st.WALWritten - st.WALSynced, SnapshotIndex: st.SnapshotIndex,
			},
			Feed: s.feeds[e.Symbol()].Stats(),
		}
		out.OrdersPerSec += rate
		out.Commands += st.Commands
		out.Fills += st.Fills
	}
	out.LatencyNs = percentiles(&all)
	if s.prev != nil {
		out.WindowMs = now.Sub(s.prevAt).Milliseconds()
	}
	s.prev, s.prevAt = cur, now

	s.mu.Lock()
	s.cur = out
	s.mu.Unlock()
}

func (s *sampler) current() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}
