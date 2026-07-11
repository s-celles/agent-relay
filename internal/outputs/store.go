// Package outputs retains the artifacts of agentic requests for later
// retrieval. Each retained request owns one directory under the store root,
// keyed by an unguessable id; entries expire after a TTL and are swept
// opportunistically on Create.
package outputs

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// ErrNotFound reports an unknown, invalid, or expired output id.
var ErrNotFound = errors.New("outputs: not found")

var validID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// NewID returns a 128-bit random identifier (32 lowercase hex chars).
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return hex.EncodeToString(b[:])
}

// FileInfo describes one retained artifact, with a path relative to the
// request's output directory.
type FileInfo struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

type Store struct {
	root string
	ttl  time.Duration
}

func New(root string, ttl time.Duration) (*Store, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("outputs root: %w", err)
	}
	return &Store{root: root, ttl: ttl}, nil
}

// Create allocates the directory for id and sweeps expired entries.
func (s *Store) Create(id string) (string, error) {
	s.Sweep(time.Now())
	dir, err := s.dir(id)
	if err != nil {
		return "", fmt.Errorf("invalid output id %q", id)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", fmt.Errorf("create output dir: %w", err)
	}
	return dir, nil
}

// Dir resolves an existing retained directory, so a caller can pin it as the
// workspace of a follow-up request.
func (s *Store) Dir(id string) (string, error) {
	dir, err := s.dir(id)
	if err != nil {
		return "", ErrNotFound
	}
	if _, err := os.Stat(dir); err != nil {
		return "", ErrNotFound
	}
	return dir, nil
}

// List returns every regular file under id, relative paths, stable order.
func (s *Store) List(id string) ([]FileInfo, error) {
	dir, err := s.dir(id)
	if err != nil {
		return nil, ErrNotFound
	}
	if _, err := os.Stat(dir); err != nil {
		return nil, ErrNotFound
	}
	var files []FileInfo
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		files = append(files, FileInfo{Path: rel, Size: info.Size()})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list outputs: %w", err)
	}
	return files, nil
}

// Open returns the artifact at the given relative path, confined to the id's
// directory. Confinement is enforced by os.Root, not by a lexical check:
// filepath.IsLocal rejects ".." and absolute paths but is blind to symlinks,
// and an agentic run writes freely into its own OutputDir. A planted link
// (leak -> ~/.claude/.credentials.json) must not be followed out of the store
// — the more so because this endpoint is gated by the *caller* token, not the
// agentic one, so following it would leak across the two privilege tiers.
func (s *Store) Open(id, rel string) (*os.File, error) {
	dir, err := s.dir(id)
	if err != nil {
		return nil, ErrNotFound
	}
	if rel == "" || !filepath.IsLocal(rel) {
		return nil, ErrNotFound
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, ErrNotFound
	}
	defer func() { _ = root.Close() }()
	// Root.Open refuses any path that escapes the root, including one that
	// traverses a symlink partway.
	f, err := root.Open(rel)
	if err != nil {
		return nil, ErrNotFound
	}
	if info, err := f.Stat(); err != nil || info.IsDir() {
		_ = f.Close()
		return nil, ErrNotFound
	}
	return f, nil
}

// Delete releases id's directory and everything under it.
func (s *Store) Delete(id string) error {
	dir, err := s.dir(id)
	if err != nil {
		return ErrNotFound
	}
	if _, err := os.Stat(dir); err != nil {
		return ErrNotFound
	}
	return os.RemoveAll(dir)
}

// Sweep removes entries older than the TTL and reports how many.
func (s *Store) Sweep(now time.Time) int {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return 0
	}
	removed := 0
	for _, e := range entries {
		if !validID.MatchString(e.Name()) {
			continue // never touch files the store did not create
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) > s.ttl {
			if os.RemoveAll(filepath.Join(s.root, e.Name())) == nil {
				removed++
			}
		}
	}
	return removed
}

func (s *Store) dir(id string) (string, error) {
	if !validID.MatchString(id) {
		return "", ErrNotFound
	}
	return filepath.Join(s.root, id), nil
}
