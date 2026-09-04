// Package store implements a minimal append-only JSONL store: every
// collection is one <collection>.jsonl file under the store directory, each
// row one JSON document + '\n'. Appends are serialized under a mutex and
// issued as a single Write to an O_APPEND handle (atomic for typical row
// sizes on local filesystems), so scans always observe whole rows.
package store

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// collectionRe validates collection names: safe path segments only.
var collectionRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Store is an append-only JSONL collection store. A Store is safe for
// concurrent Append calls.
type Store struct {
	dir   string
	mu    sync.Mutex // serializes file map access and writes
	files map[string]*os.File
}

// Open creates dir (MkdirAll) and returns a Store rooted there.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("store: create dir: %w", err)
	}
	return &Store{dir: dir, files: make(map[string]*os.File)}, nil
}

// fileLocked returns the append handle for collection, opening it on first
// use. Callers must hold s.mu.
func (s *Store) fileLocked(collection string) (*os.File, error) {
	if s.files == nil {
		// Closed store (Close nils the map): fail closed with an explicit
		// error — a late writer (e.g. a leaked poll goroutine outliving its
		// daemon) must get an error, never a nil-map panic.
		return nil, fmt.Errorf("store: closed (collection %q append dropped)", collection)
	}
	if f, ok := s.files[collection]; ok {
		return f, nil
	}
	f, err := os.OpenFile(filepath.Join(s.dir, collection+".jsonl"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("store: open collection %q: %w", collection, err)
	}
	s.files[collection] = f
	return f, nil
}

// Append marshals rec and atomically appends one JSONL row to collection.
func (s *Store) Append(collection string, rec any) error {
	if !collectionRe.MatchString(collection) {
		return fmt.Errorf("store: invalid collection name %q", collection)
	}
	row, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("store: marshal: %w", err)
	}
	row = append(row, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.fileLocked(collection)
	if err != nil {
		return err
	}
	n, err := f.Write(row)
	if err != nil {
		return fmt.Errorf("store: append %q: %w", collection, err)
	}
	if n != len(row) {
		return fmt.Errorf("store: append %q: short write (%d/%d)", collection, n, len(row))
	}
	return nil
}

// Replace atomically rewrites a collection with exactly rows (in order):
// the new content is written to <collection>.jsonl.tmp and renamed over the
// live file, and any cached append handle is closed first so the swap is
// observed whole. An empty rows slice removes the collection's contents.
// Replace enables compaction-style maintenance (e.g. the datacenter retry
// queue dropping records that have since been delivered) without ever
// exposing a half-written file to concurrent scans.
func (s *Store) Replace(collection string, rows []any) error {
	if !collectionRe.MatchString(collection) {
		return fmt.Errorf("store: invalid collection name %q", collection)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if f, ok := s.files[collection]; ok {
		if err := f.Close(); err != nil {
			return fmt.Errorf("store: replace %q: close old handle: %w", collection, err)
		}
		delete(s.files, collection)
	}
	tmp := filepath.Join(s.dir, collection+".jsonl.tmp")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("store: replace %q: open tmp: %w", collection, err)
	}
	bw := bufio.NewWriter(f)
	for _, rec := range rows {
		row, err := json.Marshal(rec)
		if err != nil {
			f.Close()
			os.Remove(tmp)
			return fmt.Errorf("store: replace %q: marshal: %w", collection, err)
		}
		bw.Write(row)
		bw.WriteByte('\n')
	}
	if err := bw.Flush(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("store: replace %q: flush: %w", collection, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("store: replace %q: close tmp: %w", collection, err)
	}
	if err := os.Rename(tmp, filepath.Join(s.dir, collection+".jsonl")); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("store: replace %q: rename: %w", collection, err)
	}
	return nil
}

// Scan streams every row of collection to fn as raw bytes (without the
// trailing newline). Scan uses its own read-only handle, so it is safe to
// call while Appends are in flight. A missing collection yields no rows and
// no error. fn's error, if any, aborts the scan and is returned.
func (s *Store) Scan(collection string, fn func(raw []byte) error) error {
	f, err := os.Open(filepath.Join(s.dir, collection+".jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("store: scan %q: %w", collection, err)
	}
	defer f.Close()
	br := bufio.NewReader(f)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			raw := bytes.TrimSuffix(line, []byte("\n"))
			if ferr := fn(raw); ferr != nil {
				return ferr
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("store: scan %q: %w", collection, err)
		}
	}
}

// Stats returns the number of rows in every *.jsonl collection under the
// store directory, keyed by collection name.
func (s *Store) Stats() map[string]int64 {
	stats := make(map[string]int64)
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return stats
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".jsonl")
		var n int64
		_ = s.Scan(name, func([]byte) error { n++; return nil })
		stats[name] = n
	}
	return stats
}

// Close flushes and closes every open collection handle. Close is idempotent.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.files == nil {
		return nil
	}
	var firstErr error
	for name, f := range s.files {
		if err := f.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("store: close %q: %w", name, err)
		}
	}
	s.files = nil
	return firstErr
}
