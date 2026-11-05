package policy

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o600)
}

// Atomic save (no temporary file left, private permissions)
// and a faithful round trip.
func TestSaveLoadCache(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	path := filepath.Join(dir, "policy.json")
	for i, doc := range []string{exampleDoc, `{"schema_version":1,"version":"p_x","models":{}}`} {
		want := Cached{ETag: exampleETag, ValidatedAt: t0.Add(-2 * time.Hour).In(time.FixedZone("x", 3600)), Document: []byte(doc)}
		if err := SaveCache(path, want); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
		got, err := LoadCache(path)
		if err != nil {
			t.Fatalf("load %d: %v", i, err)
		}
		if got.ETag != want.ETag || !got.ValidatedAt.Equal(want.ValidatedAt) {
			t.Errorf("load %d = (%q, %v)", i, got.ETag, got.ValidatedAt)
		}
		if _, err := Decode(got.Document); err != nil {
			t.Errorf("document %d: %v", i, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "policy.json" {
		t.Errorf("dir entries = %v, want only policy.json", entries)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm = %o, want 600", perm)
	}
}

func TestSaveCacheFailsWithoutReplacing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.json")
	if err := SaveCache(path, Cached{ETag: exampleETag, ValidatedAt: t0, Document: []byte(exampleDoc)}); err != nil {
		t.Fatal(err)
	}
	// A directory where the file's parent should be makes the next save fail.
	if err := SaveCache(filepath.Join(path, "policy.json"), Cached{Document: []byte(exampleDoc)}); err == nil {
		t.Fatal("err = nil, want failure")
	}
	if _, err := LoadCache(path); err != nil {
		t.Errorf("good cache damaged: %v", err)
	}
}

func TestCachePath(t *testing.T) {
	tests := []struct{ queueDir, want string }{
		{"./data/queue", filepath.Join("data", "policy.json")},
		{"/var/lib/sidecar/queue/", "/var/lib/sidecar/policy.json"},
		{"queue", "policy.json"},
	}
	for _, tt := range tests {
		if got := CachePath(tt.queueDir); got != tt.want {
			t.Errorf("CachePath(%q) = %q, want %q", tt.queueDir, got, tt.want)
		}
	}
}
