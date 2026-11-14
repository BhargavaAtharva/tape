package wal

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// A snapshot file is "TAPESNP1" | index (u64) | length (u64) | CRC-32C (u32)
// followed by the state. The index is that of the last log record the state
// includes.
const (
	snapshotMagic  = "TAPESNP1"
	snapshotHeader = 8 + 8 + 8 + 4
	snapshotPrefix = "snap-"
	snapshotSuffix = ".bin"
	tempSuffix     = ".tmp"
)

func snapshotName(index uint64) string {
	return fmt.Sprintf("%s%020d%s", snapshotPrefix, index, snapshotSuffix)
}

// WriteSnapshot stores state atomically: it is written to a temporary file,
// fsynced and renamed, so a crash leaves either the complete snapshot or
// none.
func WriteSnapshot(dir string, index uint64, state []byte) error {
	hdr := make([]byte, snapshotHeader)
	copy(hdr, snapshotMagic)
	le := binary.LittleEndian
	le.PutUint64(hdr[8:], index)
	le.PutUint64(hdr[16:], uint64(len(state)))
	le.PutUint32(hdr[24:], crc32.Checksum(state, castagnoli))

	final := filepath.Join(dir, snapshotName(index))
	tmp := final + tempSuffix
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("wal: create snapshot: %w", err)
	}
	_, err = f.Write(hdr)
	if err == nil {
		_, err = f.Write(state)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, final)
	}
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("wal: write snapshot: %w", err)
	}
	return syncDir(dir)
}

// ListSnapshots returns the indexes of the snapshot files in dir, oldest
// first, and removes temporary files left by an interrupted write.
func ListSnapshots(dir string) ([]uint64, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("wal: list dir: %w", err)
	}
	var out []uint64
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, snapshotPrefix) {
			continue
		}
		if strings.HasSuffix(name, tempSuffix) {
			os.Remove(filepath.Join(dir, name))
			continue
		}
		var index uint64
		if _, err := fmt.Sscanf(name, snapshotPrefix+"%020d"+snapshotSuffix, &index); err == nil {
			out = append(out, index)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// LoadSnapshot returns the newest snapshot that passes its checksum, trying
// older ones if newer ones are damaged. With no usable snapshot it returns
// index 0 and nil state.
func LoadSnapshot(dir string) (index uint64, state []byte, err error) {
	indexes, err := ListSnapshots(dir)
	if err != nil {
		return 0, nil, err
	}
	for i := len(indexes) - 1; i >= 0; i-- {
		state, err := readSnapshot(dir, indexes[i])
		if err != nil {
			slog.Warn("snapshot unusable, trying an older one", "index", indexes[i], "cause", err)
			continue
		}
		return indexes[i], state, nil
	}
	return 0, nil, nil
}

func readSnapshot(dir string, index uint64) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(dir, snapshotName(index)))
	if err != nil {
		return nil, err
	}
	le := binary.LittleEndian
	if len(data) < snapshotHeader || string(data[:8]) != snapshotMagic {
		return nil, errors.New("bad header")
	}
	if le.Uint64(data[8:]) != index {
		return nil, errors.New("index does not match file name")
	}
	state := data[snapshotHeader:]
	if uint64(len(state)) != le.Uint64(data[16:]) {
		return nil, errors.New("length mismatch")
	}
	if crc32.Checksum(state, castagnoli) != le.Uint32(data[24:]) {
		return nil, errors.New("checksum mismatch")
	}
	return state, nil
}

// RemoveSnapshot deletes one snapshot file.
func RemoveSnapshot(dir string, index uint64) error {
	if err := os.Remove(filepath.Join(dir, snapshotName(index))); err != nil {
		return fmt.Errorf("wal: remove snapshot: %w", err)
	}
	return nil
}
