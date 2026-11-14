package book

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// CommandSize is the length of an encoded command.
const CommandSize = 6 + 8*6

const commandVersion = 2

var ErrBadCommand = errors.New("book: malformed command record")

// AppendCommand appends the fixed-size binary encoding of c to dst. This is
// the write-ahead log's record format, so it includes Time: replay must stamp
// events exactly as the original run did.
func AppendCommand(dst []byte, c Command) []byte {
	le := binary.LittleEndian
	var flags byte
	if c.PostOnly {
		flags = 1
	}
	dst = append(dst, commandVersion, byte(c.Kind), byte(c.Side), byte(c.Type), byte(c.TIF), flags)
	dst = le.AppendUint64(dst, uint64(c.Time))
	dst = le.AppendUint64(dst, c.OrderID)
	dst = le.AppendUint64(dst, c.Trader)
	dst = le.AppendUint64(dst, uint64(c.Price))
	dst = le.AppendUint64(dst, uint64(c.Qty))
	dst = le.AppendUint64(dst, uint64(c.TTL))
	return dst
}

// DecodeCommand is the inverse of AppendCommand. It checks the framing only;
// semantically invalid commands decode fine and are rejected by Apply, which
// is exactly what happened to them the first time.
func DecodeCommand(p []byte) (Command, error) {
	if len(p) != CommandSize {
		return Command{}, fmt.Errorf("%w: %d bytes, want %d", ErrBadCommand, len(p), CommandSize)
	}
	if p[0] != commandVersion {
		return Command{}, fmt.Errorf("%w: unsupported version %d", ErrBadCommand, p[0])
	}
	le := binary.LittleEndian
	return Command{
		Kind:     CommandKind(p[1]),
		Side:     Side(p[2]),
		Type:     OrderType(p[3]),
		TIF:      TimeInForce(p[4]),
		PostOnly: p[5]&1 != 0,
		Time:     int64(le.Uint64(p[6:])),
		OrderID:  le.Uint64(p[14:]),
		Trader:   le.Uint64(p[22:]),
		Price:    int64(le.Uint64(p[30:])),
		Qty:      int64(le.Uint64(p[38:])),
		TTL:      int64(le.Uint64(p[46:])),
	}, nil
}
