package outputs

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir(), 10*time.Minute)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestNewIDFormat(t *testing.T) {
	id := NewID()
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(id) {
		t.Fatalf("id %q is not 32 lowercase hex chars", id)
	}
	if NewID() == id {
		t.Fatal("ids must not repeat")
	}
}

func TestCreateListDownloadDelete(t *testing.T) {
	s := newStore(t)
	id := NewID()
	dir, err := s.Create(id)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "result.txt"), []byte("hello"), 0o600)
	os.WriteFile(filepath.Join(dir, "sub", "data.json"), []byte("{}"), 0o600)

	files, err := s.List(id)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("files = %+v, want 2 entries", files)
	}
	byPath := map[string]int64{}
	for _, f := range files {
		byPath[f.Path] = f.Size
	}
	if byPath["result.txt"] != 5 || byPath[filepath.Join("sub", "data.json")] != 2 {
		t.Fatalf("files = %+v", files)
	}

	f, err := s.Open(id, "sub/data.json")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	data, _ := io.ReadAll(f)
	f.Close()
	if string(data) != "{}" {
		t.Errorf("content = %q", data)
	}

	if err := s.Delete(id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.List(id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("List after delete: err = %v, want ErrNotFound", err)
	}
	if err := s.Delete(id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Delete: err = %v, want ErrNotFound", err)
	}
}

func TestUnknownAndInvalidIDs(t *testing.T) {
	s := newStore(t)
	if _, err := s.List(NewID()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v, want ErrNotFound", err)
	}
	for _, bad := range []string{"", "..", "nothex!", "ABCDEF00112233445566778899aabbcc", "a/b"} {
		if _, err := s.Create(bad); err == nil {
			t.Errorf("Create(%q) should fail", bad)
		}
		if _, err := s.List(bad); !errors.Is(err, ErrNotFound) {
			t.Errorf("List(%q): %v, want ErrNotFound", bad, err)
		}
	}
}

func TestOpenRejectsTraversal(t *testing.T) {
	s := newStore(t)
	id := NewID()
	dir, _ := s.Create(id)
	os.WriteFile(filepath.Join(dir, "ok.txt"), []byte("x"), 0o600)

	for _, bad := range []string{"../secret", "/etc/passwd", "sub/../../secret", ""} {
		if _, err := s.Open(id, bad); err == nil {
			t.Errorf("Open(%q) must be refused", bad)
		}
	}
}

func TestSweepRemovesOnlyExpired(t *testing.T) {
	s := newStore(t)
	oldID, newID := NewID(), NewID()
	oldDir, _ := s.Create(oldID)
	s.Create(newID)

	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(oldDir, past, past); err != nil {
		t.Fatal(err)
	}

	if n := s.Sweep(time.Now()); n != 1 {
		t.Fatalf("Sweep removed %d entries, want 1", n)
	}
	if _, err := s.List(oldID); !errors.Is(err, ErrNotFound) {
		t.Error("expired entry should be gone")
	}
	if _, err := s.List(newID); err != nil {
		t.Errorf("fresh entry should survive: %v", err)
	}
}

func TestCreateSweepsExpired(t *testing.T) {
	s := newStore(t)
	oldID := NewID()
	oldDir, _ := s.Create(oldID)
	past := time.Now().Add(-time.Hour)
	os.Chtimes(oldDir, past, past)

	s.Create(NewID()) // creation triggers a sweep
	if _, err := s.List(oldID); !errors.Is(err, ErrNotFound) {
		t.Error("expired entry should have been swept on Create")
	}
}
