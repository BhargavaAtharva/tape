package book

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
)

func seedBook() *Book {
	b := New(3 << 40)
	for i, c := range []Command{
		limit(1, Buy, 100, 10),
		limit(2, Buy, 100, 7),
		limit(3, Buy, 98, 4),
		postOnly(4, Sell, 103, 9),
		limit(5, Sell, 101, 2),
		limit(6, Sell, 101, 6),
		limit(7, Buy, 101, 1),
		amend(2, 3<<40+2, 0, 5),
	} {
		c.Time = int64(i)
		b.Apply(c)
	}
	return b
}

func TestSnapshotRoundTrip(t *testing.T) {
	b := seedBook()
	snap := b.AppendSnapshot(nil)
	r, err := Restore(snap)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkInvariants(r); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(r.AppendSnapshot(nil), snap) {
		t.Fatal("restored book encodes differently")
	}
	if r.Hash() != b.Hash() || r.Seq() != b.Seq() {
		t.Fatal("hash or seq differs after restore")
	}

	next := []Command{
		limit(9, Sell, 100, 12),
		market(9, Buy, 3),
		cancel(3, 3<<40+3),
		limit(9, Buy, 102, 20),
	}
	for _, c := range next {
		if got, want := r.Apply(c), b.Apply(c); !reflect.DeepEqual(got, want) {
			t.Fatalf("restored book diverged\n got %+v\nwant %+v", got, want)
		}
	}
	if r.Hash() != b.Hash() {
		t.Fatal("hash differs after continuing")
	}
}

func TestHashIgnoresHowTheBookWasBuilt(t *testing.T) {
	a := New(0)
	a.Apply(limit(1, Buy, 100, 5))
	a.Apply(limit(1, Buy, 99, 5))
	a.Apply(limit(1, Buy, 98, 5))
	a.Apply(cancel(1, 2))

	r, err := Restore(a.AppendSnapshot(nil))
	if err != nil {
		t.Fatal(err)
	}
	if a.Hash() != r.Hash() {
		t.Fatal("hash depends on history")
	}
	a.Apply(limit(2, Sell, 200, 1))
	if a.Hash() == r.Hash() {
		t.Fatal("hash did not change with the book")
	}
}

func TestRestoreRejectsMalformedInput(t *testing.T) {
	good := seedBook().AppendSnapshot(nil)
	mutate := func(f func(b []byte) []byte) []byte {
		return f(bytes.Clone(good))
	}
	cases := map[string][]byte{
		"empty":         nil,
		"short header":  good[:10],
		"bad magic":     mutate(func(b []byte) []byte { b[0] ^= 0xff; return b }),
		"bad version":   mutate(func(b []byte) []byte { b[4] = 9; return b }),
		"truncated":     good[:len(good)-5],
		"trailing data": append(bytes.Clone(good), 1, 2, 3),
		"zero qty":      mutate(func(b []byte) []byte { clear(b[snapshotHeader+24 : snapshotHeader+32]); return b }),
		"duplicate id": mutate(func(b []byte) []byte {
			copy(b[snapshotHeader+snapshotOrder:], b[snapshotHeader:snapshotHeader+8])
			return b
		}),
		"price order": mutate(func(b []byte) []byte {
			b[snapshotHeader+16] = 1
			return b
		}),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Restore(data); !errors.Is(err, ErrBadSnapshot) {
				t.Fatalf("got %v, want ErrBadSnapshot", err)
			}
		})
	}
}
