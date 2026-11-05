package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const rejectedDir = "rejected"

// rejectedBatch is the on-disk form of a batch set aside by Reject. Events
// carry metadata only (CONTRIBUTING.md rule 1), so the file holds no content.
type rejectedBatch struct {
	BatchID string  `json:"batch_id"`
	Events  []Event `json:"events"`
}

// park writes b to rejected/<id>.json unless MaxRejected files are already there.
func (q *Queue) park(b Batch) error {
	if b.ID == "" || b.ID == "." || b.ID == ".." || strings.ContainsAny(b.ID, `/\`) {
		return errors.New("events: invalid batch id for rejected file")
	}
	dir := filepath.Join(q.cfg.Dir, rejectedDir)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("events: create rejected dir: %w", err)
	}
	n, err := countRejected(dir)
	if err != nil {
		return err
	}
	if n >= q.cfg.MaxRejected {
		q.rejectedDropped.Add(1)
		return nil
	}
	data, err := json.Marshal(rejectedBatch{BatchID: b.ID, Events: b.Events})
	if err != nil {
		return fmt.Errorf("events: encode rejected batch: %w", err)
	}
	if err := writeFileAtomic(dir, b.ID+".json", data); err != nil {
		return fmt.Errorf("events: rejected batch: %w", err)
	}
	q.rejected.Add(1)
	return nil
}

func countRejected(dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("events: list rejected dir: %w", err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	return n, nil
}
