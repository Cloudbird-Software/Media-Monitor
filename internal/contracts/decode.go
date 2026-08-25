package contracts

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// decode parses raw contract JSON and enforces the canonical schema
// (lightweight structural validation; full schema doc in adapt/contracts/README.json).
func decode(data []byte) (*Contract, error) {
	var c Contract
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if c.Transport.Method == "" {
		c.Transport.Method = "GET"
	}
	switch c.Transport.Method {
	case "GET", "POST":
	default:
		return nil, fmt.Errorf("unsupported method %q", c.Transport.Method)
	}
	if c.Transport.BaseURL == "" || c.Transport.Path == "" {
		return nil, fmt.Errorf("transport.base_url and transport.path are required")
	}
	if c.Binding.Items == "" && c.Binding.Comments == "" && c.Binding.Users == "" && c.Binding.Members == "" && len(c.Binding.Fields) == 0 {
		return nil, fmt.Errorf("binding must declare at least one of items/comments/users/members or fields")
	}
	if c.Paging.CountDefault <= 0 {
		c.Paging.CountDefault = 20
	}
	return &c, nil
}

// loadDir loads every *.json under dir as a contract (non-recursive).
// README.json is a human/machine documentation stub (short key/value pairs,
// not a contract schema) and is skipped. os.ReadDir returns entries sorted
// by name, so registry construction order is deterministic.
func loadDir(r *Registry, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("contracts dir: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		if strings.EqualFold(name, "README.json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		c, err := Load(name, data)
		if err != nil {
			return err
		}
		if err := r.Add(c); err != nil {
			return err
		}
	}
	return nil
}
