package netcapture

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"

	"github.com/Cloudbird-Software/Media-Monitor/internal/store"
)

// Persistence: a Manager built with NewManagerDir persists every recorded
// entry to disk via internal/store, so one-shot CLI processes can read
// sessions created by earlier runs (previously sessions lived only in process
// memory, making cross-process access impossible). Layout inside the store
// directory:
//
//   - collection "nc_sessions": one row per OpenSession call
//     {project, url, started}; the latest row for a project wins;
//   - collection "nc_<project>": one Entry JSON per row, in record order.
//
// Persisted project names must be safe store collection segments
// ([A-Za-z0-9_-]+); other names are rejected fail-closed at OpenSession.

// sessionNameRe validates project names used as store collection segments.
var sessionNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

const (
	sessionsCollection = "nc_sessions"
	entryPrefix        = "nc_"
)

// sessionMeta is one row of the nc_sessions collection.
type sessionMeta struct {
	Project string `json:"project"`
	URL     string `json:"url,omitempty"`
	Started int64  `json:"started"`
}

// NewManagerDir builds a Manager persisting sessions under dir (e.g.
// data/netcapture). Close releases the store.
func NewManagerDir(dir string) (*Manager, error) {
	st, err := store.Open(dir)
	if err != nil {
		return nil, fmt.Errorf("netcapture: open store: %w", err)
	}
	return &Manager{sessions: map[string]*Session{}, st: st}, nil
}

// Close releases the underlying store (nil for in-memory managers). It does
// not delete persisted sessions.
func (m *Manager) Close() error {
	if m.st == nil {
		return nil
	}
	return m.st.Close()
}

// openSessionPersistent is the persistent variant of OpenSession: it loads
// entries recorded by earlier processes and wires per-entry persistence.
func (m *Manager) openSessionPersistent(project, url string) (*Session, error) {
	if !sessionNameRe.MatchString(project) {
		return nil, fmt.Errorf("netcapture: project %q is not a safe store segment ([A-Za-z0-9_-]+ required)", project)
	}
	s := NewSession(project, url)
	// Reload entries recorded by earlier runs so exports accumulate across
	// processes.
	if err := m.st.Scan(entryPrefix+project, func(raw []byte) error {
		var e Entry
		if err := json.Unmarshal(raw, &e); err != nil {
			return fmt.Errorf("netcapture: decode entry: %w", err)
		}
		s.Entries = append(s.Entries, e)
		return nil
	}); err != nil {
		return nil, err
	}
	if err := m.st.Append(sessionsCollection, sessionMeta{Project: project, URL: url, Started: s.Started}); err != nil {
		return nil, fmt.Errorf("netcapture: persist session meta: %w", err)
	}
	st := m.st
	s.onEntry = func(e Entry) {
		_ = st.Append(entryPrefix+project, e)
	}
	return s, nil
}

// Load reads a persisted session from disk without opening it (recording
// stays off). It works for sessions created by other processes. The second
// return value reports whether the session exists.
func (m *Manager) Load(project string) (*Session, bool, error) {
	if m.st == nil {
		return nil, false, fmt.Errorf("netcapture: manager is not persistent (use NewManagerDir)")
	}
	var meta sessionMeta
	found := false
	if err := m.st.Scan(sessionsCollection, func(raw []byte) error {
		var row sessionMeta
		if err := json.Unmarshal(raw, &row); err == nil && row.Project == project {
			meta = row // latest row wins
			found = true
		}
		return nil
	}); err != nil {
		return nil, false, err
	}
	entries := 0
	s := &Session{Project: project, URL: meta.URL, Started: meta.Started}
	if err := m.st.Scan(entryPrefix+project, func(raw []byte) error {
		var e Entry
		if err := json.Unmarshal(raw, &e); err != nil {
			return fmt.Errorf("netcapture: decode entry: %w", err)
		}
		s.Entries = append(s.Entries, e)
		entries++
		return nil
	}); err != nil {
		return nil, false, err
	}
	if !found && entries == 0 {
		return nil, false, nil
	}
	return s, true, nil
}

// List returns the names of all known sessions: in-memory ones plus every
// persisted session (so one-shot processes see earlier runs), sorted.
func (m *Manager) List() ([]string, error) {
	names := map[string]bool{}
	m.mu.Lock()
	for name := range m.sessions {
		names[name] = true
	}
	m.mu.Unlock()
	if m.st != nil {
		if err := m.st.Scan(sessionsCollection, func(raw []byte) error {
			var row sessionMeta
			if err := json.Unmarshal(raw, &row); err == nil && row.Project != "" {
				names[row.Project] = true
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}
