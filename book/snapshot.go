package book

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	snapshotMagic   = 0x4b425054 // "TPBK" little-endian
	snapshotVersion = 1
	snapshotHeader  = 4 + 4 + 8 + 8 + 4 + 4
	snapshotOrder   = 8 + 8 + 8 + 8 + 1
)

var ErrBadSnapshot = errors.New("book: malformed snapshot")

// AppendSnapshot appends the canonical encoding of the book to dst. Orders
// are written best price first and oldest first within a price, so restoring
// them in file order rebuilds every queue exactly. Two books that would
// behave identically from here on encode to identical bytes.
func (b *Book) AppendSnapshot(dst []byte) []byte {
	le := binary.LittleEndian
	dst = le.AppendUint32(dst, snapshotMagic)
	dst = le.AppendUint32(dst, snapshotVersion)
	dst = le.AppendUint64(dst, b.seq)
	dst = le.AppendUint64(dst, b.nextID)
	dst = le.AppendUint32(dst, uint32(countOrders(&b.bids)))
	dst = le.AppendUint32(dst, uint32(countOrders(&b.asks)))
	dst = appendSide(dst, &b.bids)
	dst = appendSide(dst, &b.asks)
	return dst
}

func countOrders(s *side) int {
	n := 0
	for lv := s.best; lv != nil; lv = lv.next {
		n += lv.count
	}
	return n
}

func appendSide(dst []byte, s *side) []byte {
	le := binary.LittleEndian
	for lv := s.best; lv != nil; lv = lv.next {
		for o := lv.head; o != nil; o = o.next {
			dst = le.AppendUint64(dst, o.ID)
			dst = le.AppendUint64(dst, o.Trader)
			dst = le.AppendUint64(dst, uint64(o.Price))
			dst = le.AppendUint64(dst, uint64(o.Qty))
			var flags byte
			if o.PostOnly {
				flags = 1
			}
			dst = append(dst, flags)
		}
	}
	return dst
}

// Hash is the SHA-256 of the snapshot encoding.
func (b *Book) Hash() [sha256.Size]byte {
	return sha256.Sum256(b.AppendSnapshot(nil))
}

// Restore rebuilds a book from AppendSnapshot output. The input is validated
// structurally; integrity (checksums) is the caller's job.
func Restore(data []byte) (*Book, error) {
	le := binary.LittleEndian
	if len(data) < snapshotHeader {
		return nil, fmt.Errorf("%w: %d bytes is shorter than the header", ErrBadSnapshot, len(data))
	}
	if le.Uint32(data) != snapshotMagic {
		return nil, fmt.Errorf("%w: bad magic", ErrBadSnapshot)
	}
	if v := le.Uint32(data[4:]); v != snapshotVersion {
		return nil, fmt.Errorf("%w: unsupported version %d", ErrBadSnapshot, v)
	}
	b := New(0)
	b.seq = le.Uint64(data[8:])
	b.nextID = le.Uint64(data[16:])
	nBids := int(le.Uint32(data[24:]))
	nAsks := int(le.Uint32(data[28:]))
	body := data[snapshotHeader:]
	if len(body) != (nBids+nAsks)*snapshotOrder {
		return nil, fmt.Errorf("%w: header promises %d orders, body holds %d bytes", ErrBadSnapshot, nBids+nAsks, len(body))
	}
	if err := b.restoreSide(Buy, body[:nBids*snapshotOrder]); err != nil {
		return nil, err
	}
	if err := b.restoreSide(Sell, body[nBids*snapshotOrder:]); err != nil {
		return nil, err
	}
	if b.bids.best != nil && b.asks.best != nil && b.bids.best.price >= b.asks.best.price {
		return nil, fmt.Errorf("%w: crossed book", ErrBadSnapshot)
	}
	return b, nil
}

func (b *Book) restoreSide(sd Side, data []byte) error {
	le := binary.LittleEndian
	s := b.side(sd)
	var last *level
	for ; len(data) > 0; data = data[snapshotOrder:] {
		o := &Order{
			ID:       le.Uint64(data),
			Trader:   le.Uint64(data[8:]),
			Price:    int64(le.Uint64(data[16:])),
			Qty:      int64(le.Uint64(data[24:])),
			Side:     sd,
			PostOnly: data[32]&1 != 0,
		}
		switch {
		case o.Price <= 0 || o.Price > MaxPrice, o.Qty <= 0 || o.Qty > MaxQty, o.Trader == 0:
			return fmt.Errorf("%w: order %d has invalid fields", ErrBadSnapshot, o.ID)
		case o.ID >= b.nextID:
			return fmt.Errorf("%w: order %d is beyond the id counter", ErrBadSnapshot, o.ID)
		case b.orders[o.ID] != nil:
			return fmt.Errorf("%w: duplicate order %d", ErrBadSnapshot, o.ID)
		case last != nil && o.Price*s.sign < last.key:
			return fmt.Errorf("%w: order %d is out of price order", ErrBadSnapshot, o.ID)
		}
		if last == nil || last.price != o.Price {
			last = s.getOrCreate(o.Price)
		}
		last.pushBack(o)
		b.orders[o.ID] = o
	}
	return nil
}
