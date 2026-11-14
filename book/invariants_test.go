package book

import "fmt"

// checkInvariants walks every structure in the book and reports the first
// inconsistency. Tests call it after each command.
func checkInvariants(b *Book) error {
	total := 0
	for _, sd := range []struct {
		s    *side
		side Side
	}{{&b.bids, Buy}, {&b.asks, Sell}} {
		n, err := checkSide(b, sd.s, sd.side)
		if err != nil {
			return fmt.Errorf("%v side: %w", sd.side, err)
		}
		total += n
	}
	if total != len(b.orders) {
		return fmt.Errorf("levels hold %d orders, index holds %d", total, len(b.orders))
	}
	if b.bids.best != nil && b.asks.best != nil && b.bids.best.price >= b.asks.best.price {
		return fmt.Errorf("book crossed: bid %d >= ask %d", b.bids.best.price, b.asks.best.price)
	}
	return nil
}

func checkSide(b *Book, s *side, sd Side) (orders int, err error) {
	levels := 0
	var prev *level
	for lv := s.best; lv != nil; lv = lv.next {
		levels++
		if lv.prev != prev {
			return 0, fmt.Errorf("level %d: broken prev link", lv.price)
		}
		if prev != nil && prev.key >= lv.key {
			return 0, fmt.Errorf("level %d: list out of order", lv.price)
		}
		if lv.key != lv.price*s.sign {
			return 0, fmt.Errorf("level %d: wrong key", lv.price)
		}
		if lv.head == nil {
			return 0, fmt.Errorf("level %d: empty level left in book", lv.price)
		}
		var qty int64
		count := 0
		var po *Order
		for o := lv.head; o != nil; o = o.next {
			if o.prev != po {
				return 0, fmt.Errorf("order %d: broken prev link", o.ID)
			}
			if o.level != lv || o.Price != lv.price || o.Side != sd {
				return 0, fmt.Errorf("order %d: filed under the wrong level", o.ID)
			}
			if o.Qty <= 0 {
				return 0, fmt.Errorf("order %d: resting with qty %d", o.ID, o.Qty)
			}
			if b.orders[o.ID] != o {
				return 0, fmt.Errorf("order %d: missing from index", o.ID)
			}
			qty += o.Qty
			count++
			po = o
		}
		if lv.tail != po {
			return 0, fmt.Errorf("level %d: wrong tail", lv.price)
		}
		if qty != lv.qty || count != lv.count {
			return 0, fmt.Errorf("level %d: aggregate %d/%d, orders sum to %d/%d", lv.price, lv.qty, lv.count, qty, count)
		}
		orders += count
		prev = lv
	}
	if levels != s.n {
		return 0, fmt.Errorf("list has %d levels, counter says %d", levels, s.n)
	}
	var inorder []*level
	if err := checkTreap(s.root, &inorder); err != nil {
		return 0, err
	}
	if len(inorder) != levels {
		return 0, fmt.Errorf("treap has %d levels, list has %d", len(inorder), levels)
	}
	i := 0
	for lv := s.best; lv != nil; lv = lv.next {
		if inorder[i] != lv {
			return 0, fmt.Errorf("treap and list disagree at position %d", i)
		}
		i++
	}
	return orders, nil
}

func checkTreap(n *level, out *[]*level) error {
	if n == nil {
		return nil
	}
	if n.left != nil && (n.left.key >= n.key || n.left.prio > n.prio) {
		return fmt.Errorf("level %d: treap property broken on the left", n.price)
	}
	if n.right != nil && (n.right.key <= n.key || n.right.prio > n.prio) {
		return fmt.Errorf("level %d: treap property broken on the right", n.price)
	}
	if err := checkTreap(n.left, out); err != nil {
		return err
	}
	*out = append(*out, n)
	return checkTreap(n.right, out)
}
