// Package netcapture is the network-capture tool skeleton. The original
// software captured traffic via the embedded Chromium's CDP (Chrome DevTools
// Protocol) debugger interface (controller.netcapture.* IPC, chunk 13 UI).
// This Go rewrite provides the local capture session + HAR export; the live
// CDP wire is out of scope for a headless Go binary (it needs a browser),
// so the engine records entries fed to it and exports a standards-conformant
// HAR (HTTP Archive). This is the platform-independent core: no platform
// endpoint is reached.
//
// Sessions can be persisted to disk (NewManagerDir) so one-shot CLI
// processes can read sessions recorded by earlier runs; see persist.go.
package netcapture

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/store"
)

// Entry is one captured HTTP request/response pair.
type Entry struct {
	StartedDateTime string  `json:"startedDateTime"` // ISO 8601
	Time            int64   `json:"time"`            // ms
	Request         Req     `json:"request"`
	Response        Resp    `json:"response"`
	Timings         Timings `json:"timings"`
}

type Req struct {
	Method      string `json:"method"`
	URL         string `json:"url"`
	HTTPVersion string `json:"httpVersion"`
	Headers     []KV   `json:"headers"`
	QueryString []KV   `json:"queryString"`
	Body        string `json:"body,omitempty"`
}

type Resp struct {
	Status      int    `json:"status"`
	StatusText  string `json:"statusText"`
	HTTPVersion string `json:"httpVersion"`
	Headers     []KV   `json:"headers"`
	Body        string `json:"body,omitempty"`
}

type KV struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type Timings struct {
	Send    int64 `json:"send"`
	Wait    int64 `json:"wait"`
	Receive int64 `json:"receive"`
}

// Session is one capture session (a named recording).
type Session struct {
	mu        sync.Mutex
	Project   string  `json:"project"`
	URL       string  `json:"url,omitempty"`
	Recording bool    `json:"recording"`
	Entries   []Entry `json:"entries"`
	Started   int64   `json:"started"`
	// onEntry, when set (persistent managers), persists each recorded entry.
	onEntry func(Entry)
}

// NewSession starts a named capture session.
func NewSession(project, url string) *Session {
	return &Session{Project: project, URL: url, Started: time.Now().UnixMilli()}
}

// AddEntry records one entry (no-op when not recording).
func (s *Session) AddEntry(e Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.Recording {
		return
	}
	if e.StartedDateTime == "" {
		e.StartedDateTime = time.UnixMilli(time.Now().UnixMilli()).UTC().Format(time.RFC3339)
	}
	s.Entries = append(s.Entries, e)
	if s.onEntry != nil {
		s.onEntry(e)
	}
}

// SetRecording toggles recording.
func (s *Session) SetRecording(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Recording = on
}

// HAR exports the session as a HAR (HTTP Archive) document.
func (s *Session) HAR() HAR {
	s.mu.Lock()
	entries := make([]Entry, len(s.Entries))
	copy(entries, s.Entries)
	s.mu.Unlock()
	sort.Slice(entries, func(i, j int) bool { return entries[i].StartedDateTime < entries[j].StartedDateTime })
	return HAR{
		Log: HARLog{
			Version: "1.2",
			Creator: HARCreator{Name: "mediad-netcapture", Version: "dev"},
			Entries: entries,
		},
	}
}

// WriteHAR writes the HAR JSON to w.
func (s *Session) WriteHAR(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(s.HAR())
}

// HAR (HTTP Archive) document shape.
type HAR struct {
	Log HARLog `json:"log"`
}

type HARLog struct {
	Version string     `json:"version"`
	Creator HARCreator `json:"creator"`
	Entries []Entry    `json:"entries"`
}

type HARCreator struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Manager tracks capture sessions. With NewManager the sessions live for the
// process lifetime; with NewManagerDir every recorded entry is also persisted
// to disk (see persist.go) so one-shot processes can read earlier sessions.
type Manager struct {
	mu       sync.Mutex
	sessions map[string]*Session
	st       *store.Store // nil = in-memory only
}

// NewManager builds an in-memory Manager.
func NewManager() *Manager {
	return &Manager{sessions: map[string]*Session{}}
}

// OpenSession opens (or replaces) a named session. With a persistent manager
// it returns nil when the session cannot be persisted (fail-closed; use
// OpenSessionE to get the error).
func (m *Manager) OpenSession(project, url string) *Session {
	s, _ := m.OpenSessionE(project, url)
	return s
}

// OpenSessionE is OpenSession with an error return for persistence failures.
func (m *Manager) OpenSessionE(project, url string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.st != nil {
		s, err := m.openSessionPersistent(project, url)
		if err != nil {
			return nil, err
		}
		m.sessions[project] = s
		return s, nil
	}
	s := NewSession(project, url)
	m.sessions[project] = s
	return s, nil
}

// Session returns a session by project name.
func (m *Manager) Session(project string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[project]
	return s, ok
}

// CloseSession removes a session.
func (m *Manager) CloseSession(project string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[project]; !ok {
		return fmt.Errorf("netcapture: no session %q", project)
	}
	delete(m.sessions, project)
	return nil
}

// ValidateURL performs a basic check on a capture target URL.
func ValidateURL(url string) error {
	if url == "" {
		return errors.New("netcapture: empty url")
	}
	return nil
}
