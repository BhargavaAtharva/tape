package feed

import (
	"slices"

	"github.com/BhargavaAtharva/tape/book"
)

// ladder is one side of the L2 mirror: quantities by price plus the prices
// kept sorted best first, so the best level and a depth-limited snapshot
// need no sorting. Books here hold hundreds of levels, where a sorted slice
// with binary search beats a tree.
type ladder struct {
	qty    map[int64]int64
	prices []int64
	desc   bool
}

func newLadder(desc bool) ladder {
	return ladder{qty: make(map[int64]int64), desc: desc}
}

func (l *ladder) cmp(a, b int64) int {
	if l.desc {
		return int(b - a)
	}
	return int(a - b)
}

func (l *ladder) set(price, qty int64) {
	_, had := l.qty[price]
	switch {
	case qty == 0 && had:
		delete(l.qty, price)
		i, _ := slices.BinarySearchFunc(l.prices, price, l.cmp)
		l.prices = slices.Delete(l.prices, i, i+1)
	case qty == 0:
	case had:
		l.qty[price] = qty
	default:
		l.qty[price] = qty
		i, _ := slices.BinarySearchFunc(l.prices, price, l.cmp)
		l.prices = slices.Insert(l.prices, i, price)
	}
}

func (l *ladder) reset(levels []book.Level) {
	clear(l.qty)
	l.prices = l.prices[:0]
	for _, lv := range levels {
		l.qty[lv.Price] = lv.Qty
		l.prices = append(l.prices, lv.Price)
	}
	slices.SortFunc(l.prices, l.cmp)
}

func (l *ladder) best() (int64, bool) {
	if len(l.prices) == 0 {
		return 0, false
	}
	return l.prices[0], true
}

// top returns up to depth levels, best first; depth 0 means all.
func (l *ladder) top(depth int) [][2]int64 {
	n := len(l.prices)
	if depth > 0 && depth < n {
		n = depth
	}
	out := make([][2]int64, n)
	for i, p := range l.prices[:n] {
		out[i] = [2]int64{p, l.qty[p]}
	}
	return out
}
