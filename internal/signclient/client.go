// Package signclient implements the remote signer-service HTTP client used
// to feed platform signature parameters (a_bogus/msToken/X-Bogus/... and
// live-room websocket signatures) into collectors without shipping the
// algorithm to the client side.
package signclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Config controls the signature service client.
type Config struct {
	BaseURL string // e.g. https://sign.internal/v1
	Token   string // bearer token (optional)
	Client  *http.Client
	// ReturnUnsigned degrades to returning the original params on service
	// failure. Default false = fail closed (no unsigned requests).
	ReturnUnsigned bool
}

// Client speaks the remote signer HTTP API:
//
//	POST {BaseURL}/sign {"contract":"...","url":"...","params":{...}}
//	→ 200 {"params":{...}}  (the merged, signed parameter set)
type Client struct {
	cfg Config
	hc  *http.Client
}

// New builds a signer-service client.
func New(cfg Config) *Client {
	hc := cfg.Client
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	return &Client{cfg: cfg, hc: hc}
}

type signRequest struct {
	Contract string            `json:"contract"`
	URL      string            `json:"url"`
	Params   map[string]string `json:"params"`
}

type signResponse struct {
	Params map[string]string `json:"params"`
	Error  string            `json:"error,omitempty"`
}

// Sign implements httpclient.Signer.
func (c *Client) Sign(ctx context.Context, contractName, url string, params map[string]string) (map[string]string, error) {
	body, err := json.Marshal(signRequest{Contract: contractName, URL: url, Params: params})
	if err != nil {
		return c.degrade(params, fmt.Errorf("signclient: marshal: %w", err))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/sign", bytes.NewReader(body))
	if err != nil {
		return c.degrade(params, fmt.Errorf("signclient: request: %w", err))
	}
	req.Header.Set("Content-Type", "application/json")
	if c.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return c.degrade(params, fmt.Errorf("signclient: post: %w", err))
	}
	defer resp.Body.Close()
	var sr signResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return c.degrade(params, fmt.Errorf("signclient: decode status %d: %w", resp.StatusCode, err))
	}
	if resp.StatusCode != http.StatusOK {
		msg := sr.Error
		if msg == "" {
			msg = resp.Status
		}
		return c.degrade(params, fmt.Errorf("signclient: status %d: %s", resp.StatusCode, msg))
	}
	if len(sr.Params) == 0 {
		return c.degrade(params, fmt.Errorf("signclient: empty params in success response"))
	}
	// Pre-size with the trusted caller count only; the untrusted response
	// params merge in incrementally (no length arithmetic on mixed-trust
	// inputs, no overflow-shaped allocation).
	out := make(map[string]string, len(params))
	for k, v := range params {
		out[k] = v
	}
	for k, v := range sr.Params {
		out[k] = v
	}
	return out, nil
}

func (c *Client) degrade(params map[string]string, err error) (map[string]string, error) {
	if c.cfg.ReturnUnsigned {
		return params, nil
	}
	return nil, err
}

// WSSSignatureSigner adapts Sign into the live package SignFn shape
// (urlQuery + params → signature string), calling the remote signer with a
// synthetic contract name and reading back the "signature" param.
func (c *Client) WSSSignatureSigner(contractName string) func(urlQuery string, params map[string]string) (string, error) {
	return func(urlQuery string, params map[string]string) (string, error) {
		out, err := c.Sign(context.Background(), contractName, urlQuery, params)
		if err != nil {
			if c.cfg.ReturnUnsigned {
				return "", nil
			}
			return "", err
		}
		if sig, ok := out["signature"]; ok && sig != "" {
			return sig, nil
		}
		return "", fmt.Errorf("signclient: signer returned no signature param")
	}
}
