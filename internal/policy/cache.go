package policy

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	cacheFileName = "policy.json"
	cacheFormat   = 1
	cacheDirPerm  = 0o700
	cacheFilePerm = 0o600
)

// ErrCacheFormat is returned by LoadCache for a file it does not understand.
var ErrCacheFormat = errors.New("policy: unknown cache format")

// Cached is the on-disk copy of the last valid policy. Document is the raw,
// unfiltered body of the 200: it is filtered again on load because the YAML
// may have changed since. It holds no keys, URLs or request data.
type Cached struct {
	ETag        string
	ValidatedAt time.Time
	Document    json.RawMessage
}

type cacheFile struct {
	Format      int             `json:"format"`
	ETag        string          `json:"etag"`
	ValidatedAt time.Time       `json:"validated_at"`
	Document    json.RawMessage `json:"document"`
}

// CachePath is where the policy is cached: next to queue_dir, in its parent
// directory, so it never mixes with the queue's own files.
func CachePath(queueDir string) string {
	return filepath.Join(filepath.Dir(filepath.Clean(queueDir)), cacheFileName)
}

// SaveCache writes c to path atomically: a temporary file in the same
// directory is written, fsynced and renamed over path, and the directory is
// fsynced. A crash leaves either the old file or the new one.
func SaveCache(path string, c Cached) error {
	data, err := json.Marshal(cacheFile{Format: cacheFormat, ETag: c.ETag, ValidatedAt: c.ValidatedAt.UTC(), Document: c.Document})
	if err != nil {
		return fmt.Errorf("policy cache: encode: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, cacheDirPerm); err != nil {
		return fmt.Errorf("policy cache: %w", err)
	}
	tmp, err := os.CreateTemp(dir, cacheFileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("policy cache: %w", err)
	}
	tmpName := tmp.Name()
	if err := writeSync(tmp, data); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("policy cache: %w", err)
	}
	return syncDir(dir)
}

func writeSync(f *os.File, data []byte) error {
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Sync()
	}
	cerr := f.Close()
	if err := errors.Join(werr, cerr); err != nil {
		return fmt.Errorf("policy cache: write: %w", err)
	}
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // dir comes from the operator's queue_dir, not from a request
	if err != nil {
		return fmt.Errorf("policy cache: %w", err)
	}
	serr := d.Sync()
	cerr := d.Close()
	if err := errors.Join(serr, cerr); err != nil {
		return fmt.Errorf("policy cache: sync dir: %w", err)
	}
	return nil
}

// LoadCache reads the file written by SaveCache. It does not decode the
// document: the caller runs Decode and Filter on it.
func LoadCache(path string) (Cached, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path comes from the operator's queue_dir, not from a request
	if err != nil {
		return Cached{}, fmt.Errorf("policy cache: %w", err)
	}
	var f cacheFile
	if err := json.Unmarshal(data, &f); err != nil {
		return Cached{}, fmt.Errorf("policy cache: %w", err)
	}
	if f.Format != cacheFormat || len(f.Document) == 0 {
		return Cached{}, fmt.Errorf("%w: format %d", ErrCacheFormat, f.Format)
	}
	return Cached{ETag: f.ETag, ValidatedAt: f.ValidatedAt, Document: f.Document}, nil
}
