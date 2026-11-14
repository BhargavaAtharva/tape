package book

import "container/heap"

// expiryEntry schedules an order's expiry. Entries are not removed when an
// order fills or is cancelled; they are discarded when they reach the top
// and the order is gone. An amended order keeps its id and expiry, so its
// original entry still applies.
type expiryEntry struct {
	at int64
	id uint64
}

// expiryHeap orders entries by time, then id, so expiry order is total and
// replay emits cancellations in the same order.
type expiryHeap []expiryEntry

func (h expiryHeap) Len() int { return len(h) }
func (h expiryHeap) Less(i, j int) bool {
	return h[i].at < h[j].at || h[i].at == h[j].at && h[i].id < h[j].id
}
func (h expiryHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *expiryHeap) Push(x any)   { *h = append(*h, x.(expiryEntry)) }
func (h *expiryHeap) Pop() any {
	old := *h
	e := old[len(old)-1]
	*h = old[:len(old)-1]
	return e
}

func (h *expiryHeap) push(e expiryEntry) { heap.Push(h, e) }

// live discards stale entries and reports the earliest live one.
func (b *Book) liveExpiry() (expiryEntry, bool) {
	for b.expiry.Len() > 0 {
		e := b.expiry[0]
		if o := b.orders[e.id]; o != nil && o.ExpireAt == e.at {
			return e, true
		}
		heap.Pop(&b.expiry)
	}
	return expiryEntry{}, false
}

// NextExpiry reports the earliest time at which a resting order expires.
func (b *Book) NextExpiry() (int64, bool) {
	e, ok := b.liveExpiry()
	return e.at, ok
}

func (b *Book) expire() {
	for {
		e, ok := b.liveExpiry()
		if !ok || e.at > b.now {
			return
		}
		heap.Pop(&b.expiry)
		o := b.orders[e.id]
		b.emit(Event{Kind: EvCancelled, OrderID: o.ID, Trader: o.Trader, Side: o.Side, Price: o.Price, Qty: o.Qty, Reason: ReasonExpired})
		b.remove(o)
	}
}
