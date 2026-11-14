// Package hist is a fixed-size log-linear histogram for latency samples.
// Recording is a single atomic add, so one writer and any number of readers
// can share it without a lock.
package hist

import (
	"math/bits"
	"sync/atomic"
)

// Each power of two is split into 16 sub-buckets, which bounds the relative
// error of a reported percentile to about 6%.
const (
	subBits = 4
	sub     = 1 << subBits
	size    = (64-subBits)*sub + sub
)

type Histogram struct {
	buckets [size]atomic.Uint64
}

func index(v uint64) int {
	if v < sub {
		return int(v)
	}
	e := bits.Len64(v) - 1
	return (e-subBits+1)*sub + int(v>>(e-subBits))&(sub-1)
}

// upper is the largest value that lands in bucket i.
func upper(i int) uint64 {
	if i < sub {
		return uint64(i)
	}
	e := i/sub + subBits - 1
	m := uint64(i % sub)
	return (sub+m+1)<<(e-subBits) - 1
}

func (h *Histogram) Record(v int64) {
	if v < 0 {
		v = 0
	}
	h.buckets[index(uint64(v))].Add(1)
}

// Counts is a point-in-time copy of a histogram.
type Counts [size]uint64

func (h *Histogram) Snapshot() *Counts {
	var c Counts
	for i := range h.buckets {
		c[i] = h.buckets[i].Load()
	}
	return &c
}

// Sub returns c minus an earlier snapshot: the samples recorded in between.
func (c *Counts) Sub(earlier *Counts) *Counts {
	var d Counts
	for i := range c {
		d[i] = c[i] - earlier[i]
	}
	return &d
}

func (c *Counts) Total() uint64 {
	var n uint64
	for _, v := range c {
		n += v
	}
	return n
}

// Quantile returns an upper bound for the q-quantile (0 < q <= 1), or 0 if
// there are no samples.
func (c *Counts) Quantile(q float64) int64 {
	total := c.Total()
	if total == 0 {
		return 0
	}
	rank := uint64(q * float64(total))
	if rank < 1 {
		rank = 1
	}
	if rank > total {
		rank = total
	}
	var seen uint64
	for i, v := range c {
		seen += v
		if seen >= rank {
			return int64(upper(i))
		}
	}
	return int64(upper(size - 1))
}
