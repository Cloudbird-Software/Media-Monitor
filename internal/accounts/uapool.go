package accounts

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"time"
)

//go:embed ua-pool.json
var bundledUAPoolJSON []byte

// UAPool is a thread-safe User-Agent rotation pool. UAs are loaded from a
// JSON data file (data/ua-pool.json) that lists the original software's UA
// rotation pool. The pool yields UAs in randomized order without immediate
// repeats.
type UAPool struct {
	mu   sync.Mutex
	uas  []string
	rng  *rand.Rand
	last int
}

// NewUAPool builds a pool from a UA list. It falls back to a single generic
// desktop UA when the list is empty.
func NewUAPool(uas []string) *UAPool {
	if len(uas) == 0 {
		uas = []string{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"}
	}
	return &UAPool{uas: append([]string(nil), uas...), rng: rand.New(rand.NewSource(time.Now().UnixNano())), last: -1}
}

// LoadUAPool reads a ua-pool.json file: {"uas": ["...", ...]}.
func LoadUAPool(path string) (*UAPool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("accounts: read ua-pool %s: %w", path, err)
	}
	var doc struct {
		UAs []string `json:"uas"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("accounts: parse ua-pool %s: %w", path, err)
	}
	return NewUAPool(doc.UAs), nil
}

// DefaultUAPoolPath is the production UA-pool location: data/ua-pool.json
// next to the running executable.
func DefaultUAPoolPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("accounts: locate executable: %w", err)
	}
	return filepath.Join(filepath.Dir(exe), "data", "ua-pool.json"), nil
}

// LoadUAPoolDefault loads the production UA pool: explicitPath when
// non-empty, otherwise the executable-relative data/ua-pool.json. If no file
// is present (the data/ dir is gitignored in source), it falls back to the
// ua-pool.json compiled into the binary via go:embed.
func LoadUAPoolDefault(explicitPath string) (*UAPool, error) {
	path := explicitPath
	if path == "" {
		var err error
		path, err = DefaultUAPoolPath()
		if err != nil {
			return nil, err
		}
	}
	if _, err := os.Stat(path); err == nil {
		return LoadUAPool(path)
	}
	return BundledUAPool()
}

// BundledUAPool returns the UA pool compiled into the binary (the 44-entry
// pool extracted verbatim from the original software's ua.js). This is the
// guaranteed-available fallback when no data/ua-pool.json exists next to the
// executable.
func BundledUAPool() (*UAPool, error) {
	var doc struct {
		UAs []string `json:"uas"`
	}
	if err := json.Unmarshal(bundledUAPoolJSON, &doc); err != nil {
		return nil, fmt.Errorf("accounts: parse bundled ua-pool: %w", err)
	}
	return NewUAPool(doc.UAs), nil
}

// Next returns the next UA in rotation, avoiding an immediate repeat when the
// pool has more than one entry.
func (p *UAPool) Next() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.uas) == 0 {
		return ""
	}
	if len(p.uas) == 1 {
		return p.uas[0]
	}
	idx := p.rng.Intn(len(p.uas))
	if idx == p.last {
		idx = (idx + 1) % len(p.uas)
	}
	p.last = idx
	return p.uas[idx]
}

// Pick returns a uniformly random UA (used to assign a stable per-account UA).
func (p *UAPool) Pick() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.uas) == 0 {
		return ""
	}
	return p.uas[p.rng.Intn(len(p.uas))]
}

// Len returns the number of UAs in the pool.
func (p *UAPool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.uas)
}
