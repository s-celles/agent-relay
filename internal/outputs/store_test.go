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

func TestOpenRefusesSymlinkEscape(t *testing.T) {
	// The lexical IsLocal check cannot see that a path component is a symlink.
	// An agentic run with a broad enough toolset can plant one in its own
	// output dir pointing at a host file the relay uid can read; a later
	// download must not follow it. Worse, the download endpoint is gated by
	// the caller token, not the agentic one — so following the link leaks
	// across the two privilege tiers the design keeps separate.
	secret := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(secret, []byte("SUBSCRIPTION-TOKEN"), 0o600); err != nil {
		t.Fatal(err)
	}

	s := newStore(t)
	id := NewID()
	dir, _ := s.Create(id)

	// A direct link, and a link nested one directory down.
	if err := os.Symlink(secret, filepath.Join(dir, "leak")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "sub", "leak")); err != nil {
		t.Fatal(err)
	}
	// A link to a *directory*, to escape and then descend.
	if err := os.Symlink(filepath.Dir(secret), filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}

	for _, rel := range []string{"leak", "sub/leak", "escape/credentials"} {
		f, err := s.Open(id, rel)
		if err == nil {
			data, _ := io.ReadAll(f)
			f.Close()
			t.Errorf("Open(%q) followed a symlink out of the store and read %q", rel, data)
		}
	}
}

func TestListDoesNotFollowSymlinkedDir(t *testing.T) {
	// Enumeration must not descend a planted directory symlink and report
	// host files as if they were retained artifacts.
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "host-file"), []byte("x"), 0o600)

	s := newStore(t)
	id := NewID()
	dir, _ := s.Create(id)
	os.WriteFile(filepath.Join(dir, "real.txt"), []byte("x"), 0o600)
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	files, err := s.List(id)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, f := range files {
		if f.Path == filepath.Join("escape", "host-file") {
			t.Errorf("List descended a symlinked dir and exposed %q", f.Path)
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
