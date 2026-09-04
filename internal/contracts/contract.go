// Package contracts implements the contract-driven collection model:
// every platform behavior (endpoint, params, headers, signing requirements,
// response binding, pagination) is declared as a versioned JSON contract
// under adapt/contracts/. Code in this package never contains endpoint URLs.
package contracts

import (
	"fmt"
	"sort"
)

// Contract declares one collection operation against one platform endpoint.
type Contract struct {
	Name         string            `json:"name"`
	Platform     string            `json:"platform"` // douyin|kuaishou|xhs|generic
	Category     string            `json:"category"` // search|items|comments|replies|user|group_members|live_meta
	Version      string            `json:"version"`
	Doc          string            `json:"doc,omitempty"`
	Transport    Transport         `json:"transport"`
	Signature    Signature         `json:"signature,omitempty"`
	Binding      Binding           `json:"binding"`
	TransportWS  *TransportWS      `json:"transport_ws,omitempty"`
	ProtoMethods map[string]string `json:"protocol_methods,omitempty"`
	Paging       Paging            `json:"paging,omitempty"`
	Cookie       CookieSpec        `json:"cookie,omitempty"`
}

// TransportWS declares a websocket endpoint shape (path + fixed query
// params + runtime-param names) for live monitors.
type TransportWS struct {
	WSSHost       string            `json:"wss_host,omitempty"`
	Path          string            `json:"path"`
	Params        map[string]string `json:"params,omitempty"`
	RuntimeParams []string          `json:"runtime_params,omitempty"`
}

type Transport struct {
	BaseURL      string            `json:"base_url"`
	Path         string            `json:"path"` // may contain {placeholders}
	Method       string            `json:"method"`
	Query        map[string]string `json:"query,omitempty"` // static query params
	Headers      map[string]string `json:"headers,omitempty"`
	Body         map[string]any    `json:"body,omitempty"`         // static JSON body fields
	Placeholders []string          `json:"placeholders,omitempty"` // required path placeholders, e.g. ["aweme_id"]
	// AltHosts lists additional accepted hosts for URL validation (e.g. live
	// room URL aliases); the base_url host is always accepted.
	AltHosts []string `json:"alt_hosts,omitempty"`
	// (silent-scraping TODO-C resolved 2026-09: the A-line corpus verdict
	// named the xhs reply-target parameter root_comment_id, 64/64 — the
	// transitional transport.reply_target_param override was removed; the
	// parameter name now rides Placeholders[0] as plain contract data.)
}

type Signature struct {
	// Params that must be present in the final query (computed by the
	// Signer implementation). "required" lists the ones a canary must
	// observe present in live traffic.
	Params   []string `json:"params,omitempty"`
	Required []string `json:"required,omitempty"`
	// Headers lists signer-produced values that must ride HTTP request
	// headers instead of the URL query (IFACE-7, e.g. xhs x-s /
	// x-s-common). Required values declared here are validated against
	// the signer output, not the query.
	Headers []string `json:"headers,omitempty"`
}

type Binding struct {
	// JSONPath bindings from the raw response into collector output
	// fields. Paths use the tokens below.
	Items         string            `json:"items"` // e.g. "$.data"
	ItemID        string            `json:"item_id"`
	ItemDesc      string            `json:"item_desc,omitempty"`
	ItemMediaType string            `json:"item_media_type,omitempty"`
	ItemAuthor    string            `json:"item_author,omitempty"`
	Comments      string            `json:"comments,omitempty"`
	ReplyCID      string            `json:"reply_cid,omitempty"`
	Users         string            `json:"users,omitempty"`
	Members       string            `json:"members,omitempty"`
	Fields        map[string]string `json:"fields,omitempty"` // named output → path
	Default       map[string]any    `json:"default,omitempty"`
}

type Paging struct {
	CursorParam    string `json:"cursor_param,omitempty"`
	CountParam     string `json:"count_param,omitempty"`
	CountDefault   int    `json:"count_default,omitempty"`
	HasMorePath    string `json:"has_more_path,omitempty"`
	NextCursorPath string `json:"next_cursor_path,omitempty"`
	// PageSleepMS overrides the engine's inter-page think-time median for
	// this contract (silent-scraping pacing): 0 = inherit the global config,
	// -1 = pacing off for this contract, >0 = median in milliseconds.
	PageSleepMS int `json:"page_sleep_ms,omitempty"`
}

type CookieSpec struct {
	Required []string `json:"required,omitempty"` // cookie names required for 200
	// Source describes where a caller must obtain them (browser profile,
	// env var, task config). Kept declarative: no values live here.
	Source string `json:"source,omitempty"`
}

// Registry loads and indexes contracts by name.
type Registry struct {
	byName map[string]*Contract
	names  []string
}

// Load parses a contract JSON document (filename included in errors).
func Load(name string, data []byte) (*Contract, error) {
	c, err := decode(data)
	if err != nil {
		return nil, fmt.Errorf("contract %s: %w", name, err)
	}
	if c.Name == "" {
		c.Name = name
	}
	return c, nil
}

func NewRegistry() *Registry { return &Registry{byName: map[string]*Contract{}} }

func (r *Registry) Add(c *Contract) error {
	if c == nil || c.Name == "" {
		return fmt.Errorf("contract with empty name")
	}
	if _, dup := r.byName[c.Name]; dup {
		return fmt.Errorf("duplicate contract %q", c.Name)
	}
	r.byName[c.Name] = c
	r.names = append(r.names, c.Name)
	sort.Strings(r.names)
	return nil
}

func (r *Registry) Get(name string) (*Contract, bool) { c, ok := r.byName[name]; return c, ok }
func (r *Registry) List() []string                    { return append([]string(nil), r.names...) }

// Platform returns all contracts for a platform, ordered by name.
func (r *Registry) Platform(p string) []*Contract {
	var out []*Contract
	for _, n := range r.names {
		if c := r.byName[n]; c.Platform == p {
			out = append(out, c)
		}
	}
	return out
}

// LoadDir loads every *.json under dir as a contract (non-recursive).
func LoadDir(r *Registry, dir string) error {
	return loadDir(r, dir)
}
