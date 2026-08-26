// Package accounts is the platform-independent account core: the account
// model, a storage-backed account pool, and cookie import/export (Netscape
// cookie.txt and JSON). Platform differences (which cookie names matter, UA
// hints) live in internal/platforms; this package never mentions a specific
// platform name.
//
// An account bundles everything the collect engine needs to act as one
// identity: its cookie set, an optional per-account HTTP proxy, and an
// optional pinned User-Agent. The engine resolves an "account context"
// (account -> cookie + proxy + UA) in one place; without an account it falls
// back to the platform-level defaults (backward compatible).
package accounts

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/store"
)

// Status is the lifecycle of an account in the pool.
type Status string

const (
	StatusActive Status = "active"
	StatusPaused Status = "paused"
	StatusBanned Status = "banned"
)

// Account is one identity the engine can act as. All fields are best-effort:
// an account may carry only cookies, only a proxy, or any combination. The
// engine merges account-level values over platform-level defaults.
type Account struct {
	ID       string            `json:"id"`       // opaque pool id
	Platform string            `json:"platform"` // douyin|kuaishou|xhs
	Nickname string            `json:"nickname,omitempty"`
	Cookies  map[string]string `json:"cookies"`         // name=value pairs
	Proxy    string            `json:"proxy,omitempty"` // http://user:pass@host:port
	UA       string            `json:"ua,omitempty"`    // pinned User-Agent
	Tags     []string          `json:"tags,omitempty"`
	Status   Status            `json:"status"`
	// Probe-derived health (W4-C1): empty = never probed. Persisted with the
	// snapshot so it survives restarts.
	Health          Health `json:"health,omitempty"`
	HealthCheckedAt int64  `json:"health_checked_at,omitempty"`
	HealthDetail    string `json:"health_detail,omitempty"`
	CreatedAt       int64  `json:"created_at"`
	UpdatedAt       int64  `json:"updated_at"`
}

// CookieHeader renders the cookie set as a "k1=v1; k2=v2" header fragment.
func (a *Account) CookieHeader() string {
	if len(a.Cookies) == 0 {
		return ""
	}
	// Deterministic order: sorted keys make the header stable.
	keys := make([]string, 0, len(a.Cookies))
	for k := range a.Cookies {
		keys = append(keys, k)
	}
	sortStrings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+a.Cookies[k])
	}
	return strings.Join(parts, "; ")
}

// sortStrings sorts a string slice in place (stdlib only — avoid importing sort
// just for this when the slice is tiny, but keep it correct).
func sortStrings(s []string) {
	// insertion sort: cookie sets are small (dozens at most).
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// Pool is a storage-backed account pool. The store collection "accounts"
// holds one JSON document per account (the Account struct). A Pool is safe for
// concurrent use.
type Pool struct {
	mu     sync.Mutex
	st     *store.Store
	cache  map[string]Account // id -> account (loaded on demand / write)
	loaded bool               // snapshot hydrated at least once
}

// Open opens (or creates) the account pool rooted at dir.
func Open(dir string) (*Pool, error) {
	st, err := store.Open(dir)
	if err != nil {
		return nil, err
	}
	return &Pool{st: st, cache: map[string]Account{}}, nil
}

// Close flushes the underlying store.
func (p *Pool) Close() error {
	return p.st.Close()
}

// List returns every account, ordered by id.
func (p *Pool) List() []Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.loadAll()
	out := make([]Account, 0, len(p.cache))
	for _, a := range p.cache {
		out = append(out, a)
	}
	sortAccounts(out)
	return out
}

// Get returns one account by id.
func (p *Pool) Get(id string) (Account, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.loadAll()
	a, ok := p.cache[id]
	return a, ok
}

// ActiveFor returns the active accounts for a platform, ordered by id.
func (p *Pool) ActiveFor(platform string) []Account {
	var out []Account
	for _, a := range p.List() {
		if a.Platform == platform && a.Status == StatusActive {
			out = append(out, a)
		}
	}
	return out
}

// Save inserts or updates an account. The id is required; CreatedAt/UpdatedAt
// are maintained automatically.
func (p *Pool) Save(a Account) error {
	if a.ID == "" {
		return fmt.Errorf("accounts: account id is required")
	}
	now := time.Now().Unix()
	if a.CreatedAt == 0 {
		a.CreatedAt = now
	}
	a.UpdatedAt = now
	if a.Status == "" {
		a.Status = StatusActive
	}
	if a.Cookies == nil {
		a.Cookies = map[string]string{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cache[a.ID] = a
	return p.persist()
}

// Delete removes an account by id.
func (p *Pool) Delete(id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.cache[id]; !ok {
		return fmt.Errorf("accounts: %q not found", id)
	}
	delete(p.cache, id)
	return p.persist()
}

// persist writes the whole pool as a single JSON array (small N; atomic enough
// for a local pool). Replaces the collection contents each time.
func (p *Pool) persist() error {
	p.loadAll() // hydrate pre-existing snapshot rows before the rewrite
	docs := make([]Account, 0, len(p.cache))
	for _, a := range p.cache {
		docs = append(docs, a)
	}
	sortAccounts(docs)
	// store.Append only adds; rewrite by marshalling the full snapshot as one
	// row under collection "accounts_snapshot" and trusting loadAll to read it.
	// (The pool is small; a single-row snapshot keeps the store append-only.)
	row, err := json.Marshal(docs)
	if err != nil {
		return fmt.Errorf("accounts: marshal: %w", err)
	}
	// Best-effort truncate: reopen the snapshot file by writing through a
	// sidecar. Simpler: append a new row and let loadAll take the last one.
	return p.st.Append("accounts_snapshot", json.RawMessage(row))
}

// loadAll hydrates the cache from the latest snapshot row. Cheap and idempotent
// because the pool is small; callers hold p.mu.
func (p *Pool) loadAll() {
	if p.loaded {
		return
	}
	var latest []Account
	_ = p.st.Scan("accounts_snapshot", func(raw []byte) error {
		var docs []Account
		if err := json.Unmarshal(raw, &docs); err == nil {
			latest = docs
		}
		return nil
	})
	for _, a := range latest {
		p.cache[a.ID] = a
	}
	p.loaded = true
}

func sortAccounts(s []Account) {
	// insertion sort on id; pool is small.
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1].ID > s[j].ID; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// ---- cookie import / export ----

// ImportCookiesNetscape parses a Netscape cookie.txt (Mozilla export) and
// returns name=value pairs. Lines starting with "#" or blank lines are
// ignored. Format: domain  INCLUDED  path  secure  expires  name  value.
func ImportCookiesNetscape(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 7 {
			return nil, fmt.Errorf("accounts: netscape: bad line %q", line)
		}
		name := fields[5]
		value := strings.Join(fields[6:], " ")
		out[name] = value
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("accounts: netscape: scan: %w", err)
	}
	return out, nil
}

// ImportCookiesJSON parses a JSON cookie export. Accepted shapes:
//   - an object {"name": "value", ...}
//   - a list of {"name":..., "value":..., ...} objects
func ImportCookiesJSON(r io.Reader) (map[string]string, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("accounts: json: read: %w", err)
	}
	out := map[string]string{}
	// Try object form first.
	if err := json.Unmarshal(data, &out); err == nil {
		return out, nil
	}
	// List form.
	var list []map[string]any
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("accounts: json: expected an object {\"name\":\"value\"} or a list of {name,value}: %w", err)
	}
	for i, item := range list {
		name, _ := item["name"].(string)
		if name == "" {
			return nil, fmt.Errorf("accounts: json: entry %d: missing name", i)
		}
		value, _ := item["value"].(string)
		out[name] = value
	}
	return out, nil
}

// ExportCookiesNetscape writes cookies in Netscape cookie.txt format. The
// domain argument tags every cookie (use ".example.com" for host-only false).
func ExportCookiesNetscape(w io.Writer, domain string, cookies map[string]string) error {
	if domain == "" {
		domain = ".example.com"
	}
	keys := make([]string, 0, len(cookies))
	for k := range cookies {
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, k := range keys {
		if _, err := fmt.Fprintf(w, "%s\tTRUE\t/\tFALSE\t0\t%s\t%s\n", domain, k, cookies[k]); err != nil {
			return err
		}
	}
	return nil
}

// ExportCookiesJSON writes cookies as a {"name":"value"} object.
func ExportCookiesJSON(w io.Writer, cookies map[string]string) error {
	enc := json.NewEncoder(w)
	return enc.Encode(cookies)
}

// parseCookieHeader splits a "k1=v1; k2=v2" header fragment into pairs.
func parseCookieHeader(h string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(h, ";") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		if name := strings.TrimSpace(kv[0]); name != "" {
			out[name] = strings.TrimSpace(kv[1])
		}
	}
	return out
}

// atoi is a thin strconv.Atoi wrapper that returns 0 on error.
func atoi(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}
