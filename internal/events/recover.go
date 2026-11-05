package events

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

const (
	cursorFile  = "ack.json"
	pendingFile = "pending.json"
)

// ackCursor is the position just past the last acknowledged event.
type ackCursor struct {
	Segment uint64 `json:"segment"`
	Offset  int64  `json:"offset"`
	BatchID string `json:"batch_id"`
}

func (c ackCursor) covers(seg uint64, end int64) bool {
	return seg < c.Segment || (seg == c.Segment && end <= c.Offset)
}

func readCursor(dir string) (ackCursor, error) {
	var c ackCursor
	data, err := os.ReadFile(filepath.Join(dir, cursorFile)) //nolint:gosec // fixed file name inside the queue dir from the sidecar config
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, fmt.Errorf("events: read ack cursor: %w", err)
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("events: decode ack cursor: %w", err)
	}
	return c, nil
}

// writeCursor replaces the cursor atomically.
func writeCursor(dir string, c ackCursor) error {
	data, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("events: encode ack cursor: %w", err)
	}
	if err := writeFileAtomic(dir, cursorFile, data); err != nil {
		return fmt.Errorf("events: ack cursor: %w", err)
	}
	return nil
}

// pendingMarker is the outstanding batch: its ID and the position just past
// its last event. It lets a restart redeliver the same batch under the same ID.
type pendingMarker struct {
	BatchID string `json:"batch_id"`
	Segment uint64 `json:"segment"`
	Offset  int64  `json:"offset"`
}

func writePending(dir string, p pendingMarker) error {
	data, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("events: encode pending batch: %w", err)
	}
	if err := writeFileAtomic(dir, pendingFile, data); err != nil {
		return fmt.Errorf("events: pending batch: %w", err)
	}
	return nil
}

// readPending returns ok = false when there is no usable marker. A corrupt
// marker is ignored: the events are still redelivered, under a new batch_id.
func readPending(dir string) (pendingMarker, bool) {
	var p pendingMarker
	data, err := os.ReadFile(filepath.Join(dir, pendingFile)) //nolint:gosec // fixed file name inside the queue dir from the sidecar config
	if err != nil || json.Unmarshal(data, &p) != nil || p.BatchID == "" {
		return pendingMarker{}, false
	}
	return p, true
}

// writeFileAtomic replaces dir/name: temp file, fsync, rename, fsync dir.
func writeFileAtomic(dir, name string, data []byte) error {
	tmp := filepath.Join(dir, name+".tmp")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, filePerm) //nolint:gosec // name is fixed or a ULID, inside the queue dir from the sidecar config
	if err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write: %w", err)
	}
	if err := syncClose(f); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // dir is the queue directory from the sidecar config, not user input
	if err != nil {
		return fmt.Errorf("events: open queue dir: %w", err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("events: fsync queue dir: %w", err)
	}
	return nil
}

func listSegments(dir string) ([]uint64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("events: list queue dir: %w", err)
	}
	segs := make([]uint64, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "seg-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		seq, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(name, "seg-"), ".jsonl"), 10, 64)
		if err != nil {
			continue
		}
		segs = append(segs, seq)
	}
	slices.Sort(segs)
	return segs, nil
}

// recover loads unacknowledged events from disk. A final line without a
// newline (torn write) or any line that is not a valid event is discarded and
// counted in CorruptLines. Segments with nothing left to deliver are removed.
func (q *Queue) recover() error {
	cur, err := readCursor(q.cfg.Dir)
	if err != nil {
		return err
	}
	segs, err := listSegments(q.cfg.Dir)
	if err != nil {
		return err
	}
	// New segments must sort after the cursor even if every acked segment is gone.
	q.seg = cur.Segment
	for _, s := range segs {
		q.seg = max(q.seg, s)
		path := filepath.Join(q.cfg.Dir, segmentName(s))
		var recs []record
		if s >= cur.Segment {
			if recs, err = q.readSegment(path, s, cur); err != nil {
				return err
			}
		}
		if len(recs) == 0 {
			if err := os.Remove(path); err != nil {
				return fmt.Errorf("events: remove drained segment: %w", err)
			}
			continue
		}
		q.segs = append(q.segs, s)
		q.recs = append(q.recs, recs...)
		for _, r := range recs {
			q.used += r.size
		}
	}
	q.restorePending(cur)
	return nil
}

// restorePending rebuilds the batch that was outstanding before the restart
// with its original ID, limited to the events that survived on disk.
func (q *Queue) restorePending(cur ackCursor) {
	p, ok := readPending(q.cfg.Dir)
	if !ok || cur.covers(p.Segment, p.Offset) {
		return
	}
	n := 0
	for n < len(q.recs) && (q.recs[n].seg < p.Segment || (q.recs[n].seg == p.Segment && q.recs[n].end <= p.Offset)) {
		n++
	}
	if n == 0 {
		return
	}
	evs := make([]Event, n)
	for i := range n {
		evs[i] = q.recs[i].ev
	}
	q.pending = &pendingBatch{batch: Batch{ID: p.BatchID, Events: evs}, n: n}
}

func (q *Queue) readSegment(path string, seg uint64, cur ackCursor) ([]record, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is built from the queue dir and a parsed segment number
	if err != nil {
		return nil, fmt.Errorf("events: read segment: %w", err)
	}
	var recs []record
	var off int64
	for off < int64(len(data)) {
		i := bytes.IndexByte(data[off:], '\n')
		if i < 0 { // torn final line
			q.corrupt.Add(1)
			break
		}
		line := data[off : off+int64(i)]
		end := off + int64(i) + 1
		size := end - off
		off = end
		if cur.covers(seg, end) {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			q.corrupt.Add(1)
			continue
		}
		recs = append(recs, record{ev: ev, seg: seg, end: end, size: size})
	}
	return recs, nil
}
