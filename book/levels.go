package book

// Order is a resting order. Qty is the open quantity.
type Order struct {
	ID       uint64
	Trader   uint64
	Price    int64
	Qty      int64
	Side     Side
	PostOnly bool

	prev, next *Order
	level      *level
	// tprev/tnext link the trader's resting orders on this book.
	tprev, tnext *Order
}

// level is a FIFO queue of orders at one price. It is simultaneously a node
// in a treap (left/right/prio) for O(log n) lookup and in a sorted doubly
// linked list (prev/next) so the next-best level is one pointer away.
type level struct {
	price int64
	key   int64
	qty   int64
	count int

	head, tail *Order

	prev, next  *level
	left, right *level
	prio        uint64
}

func (l *level) pushBack(o *Order) {
	o.level = l
	o.prev = l.tail
	o.next = nil
	if l.tail != nil {
		l.tail.next = o
	} else {
		l.head = o
	}
	l.tail = o
	l.qty += o.Qty
	l.count++
}

func (l *level) unlink(o *Order) {
	if o.prev != nil {
		o.prev.next = o.next
	} else {
		l.head = o.next
	}
	if o.next != nil {
		o.next.prev = o.prev
	} else {
		l.tail = o.prev
	}
	o.prev, o.next, o.level = nil, nil, nil
	l.qty -= o.Qty
	l.count--
}

// side holds every level of one side of the book, ordered by key. Asks use
// key = price and bids key = -price, so for both sides the best level has
// the smallest key and sits at the head of the list.
type side struct {
	root *level
	best *level
	sign int64
	n    int
	free *level
	// key for the treap priority hash; see mix.
	seed uint64
}

func (s *side) getOrCreate(price int64) *level {
	key := price * s.sign
	if s.best != nil && s.best.key == key {
		return s.best
	}
	var pred, succ *level
	n := s.root
	for n != nil {
		switch {
		case key < n.key:
			succ = n
			n = n.left
		case key > n.key:
			pred = n
			n = n.right
		default:
			return n
		}
	}
	lv := s.free
	if lv != nil {
		s.free = lv.next
		*lv = level{}
	} else {
		lv = &level{}
	}
	lv.price, lv.key, lv.prio = price, key, mix(uint64(price)^s.seed)
	lv.prev, lv.next = pred, succ
	if pred != nil {
		pred.next = lv
	} else {
		s.best = lv
	}
	if succ != nil {
		succ.prev = lv
	}
	s.root = treapInsert(s.root, lv)
	s.n++
	return lv
}

func (s *side) remove(lv *level) {
	if lv.prev != nil {
		lv.prev.next = lv.next
	} else {
		s.best = lv.next
	}
	if lv.next != nil {
		lv.next.prev = lv.prev
	}
	s.root = treapRemove(s.root, lv.key)
	s.n--
	*lv = level{next: s.free}
	s.free = lv
}

// mix is the splitmix64 finalizer. A treap priority is mix(price ^ seed),
// with a random seed per book. It needs no RNG state, and because the seed
// is secret a client cannot choose prices that degrade the treap into a
// list. The tree's shape never affects matching, so a recovered book with a
// different seed still produces exactly the same events.
func mix(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

func rotateRight(n *level) *level {
	l := n.left
	n.left = l.right
	l.right = n
	return l
}

func rotateLeft(n *level) *level {
	r := n.right
	n.right = r.left
	r.left = n
	return r
}

func treapInsert(root, n *level) *level {
	if root == nil {
		return n
	}
	if n.key < root.key {
		root.left = treapInsert(root.left, n)
		if root.left.prio > root.prio {
			root = rotateRight(root)
		}
	} else {
		root.right = treapInsert(root.right, n)
		if root.right.prio > root.prio {
			root = rotateLeft(root)
		}
	}
	return root
}

func treapRemove(root *level, key int64) *level {
	if root == nil {
		return nil
	}
	switch {
	case key < root.key:
		root.left = treapRemove(root.left, key)
	case key > root.key:
		root.right = treapRemove(root.right, key)
	default:
		if root.left == nil {
			return root.right
		}
		if root.right == nil {
			return root.left
		}
		if root.left.prio > root.right.prio {
			root = rotateRight(root)
			root.right = treapRemove(root.right, key)
		} else {
			root = rotateLeft(root)
			root.left = treapRemove(root.left, key)
		}
	}
	return root
}
