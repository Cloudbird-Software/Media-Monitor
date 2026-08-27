// GitHub notifier: opens/closes drift issues through the repo App token
// (AGENT_APP_SECRET, app id fixed). stdlib-only: the RS256 JWT is signed
// with crypto/rsa directly.
package waterlevel

import (
	"bytes"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// AppID is the org's write identity app (docs/CANARY.md, fixed).
const AppID = "4632704"

// GitHubNotifier files issues in one repo via an App installation token.
type GitHubNotifier struct {
	Repo         string // owner/name
	InstallToken string // pre-minted ghs_ token
	Client       *http.Client
}

// NewGitHubNotifier mints an installation token from the AGENT_APP_SECRET
// PEM (env) for the repo's installation.
func NewGitHubNotifier(repo, installationID string) (*GitHubNotifier, error) {
	pemRaw := os.Getenv("AGENT_APP_SECRET")
	if strings.TrimSpace(pemRaw) == "" {
		return nil, fmt.Errorf("waterlevel: AGENT_APP_SECRET not set — issue filing disabled (fail-closed)")
	}
	key, err := parseRSAPrivateKey([]byte(pemRaw))
	if err != nil {
		return nil, fmt.Errorf("waterlevel: AGENT_APP_SECRET: %w", err)
	}
	jwtTok, err := appJWT(key)
	if err != nil {
		return nil, err
	}
	tok, err := installationToken(jwtTok, installationID)
	if err != nil {
		return nil, err
	}
	return &GitHubNotifier{Repo: repo, InstallToken: tok, Client: &http.Client{Timeout: 30 * time.Second}}, nil
}

func parseRSAPrivateKey(raw []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("no PEM block")
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

// appJWT builds the app's RS256 JWT (stdlib only).
func appJWT(key *rsa.PrivateKey) (string, error) {
	b64 := func(b []byte) string { return strings.TrimRight(base64.URLEncoding.EncodeToString(b), "=") }
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	now := time.Now().Unix()
	payload, _ := json.Marshal(map[string]any{"iat": now - 60, "exp": now + 300, "iss": AppID})
	signing := b64(header) + "." + b64(payload)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(nil, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + b64(sig), nil
}

// installationToken exchanges the app JWT for a repo installation token.
func installationToken(jwtTok, installationID string) (string, error) {
	body, _ := json.Marshal(map[string]any{})
	req, err := http.NewRequest(http.MethodPost,
		fmt.Sprintf("https://api.github.com/app/installations/%s/access_tokens", installationID), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+jwtTok)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("installation token: status %d: %s", resp.StatusCode, b)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", fmt.Errorf("installation token: empty token")
	}
	return out.Token, nil
}

func (g *GitHubNotifier) gh(method, path string, body any) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "https://api.github.com"+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+g.InstallToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.Client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b, nil
}

// OpenIssue files the drift issue (label type:drift).
func (g *GitHubNotifier) OpenIssue(a Alert) (int, error) {
	code, b, err := g.gh(http.MethodPost, fmt.Sprintf("/repos/%s/issues", g.Repo), map[string]any{
		"title": a.IssueTitle(), "body": a.IssueBody(), "labels": []string{"type:drift"},
	})
	if err != nil {
		return 0, err
	}
	if code != http.StatusCreated {
		return 0, fmt.Errorf("open issue: status %d: %s", code, b)
	}
	var out struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.Number == 0 {
		return 0, fmt.Errorf("open issue: bad response: %s", b)
	}
	return out.Number, nil
}

// OpenIssueNumber finds an open water-level drift issue for a platform.
func (g *GitHubNotifier) OpenIssueNumber(platform string) (int, error) {
	code, b, err := g.gh(http.MethodGet,
		fmt.Sprintf("/repos/%s/issues?state=open&labels=type%%3Adrift&per_page=100", g.Repo), nil)
	if err != nil {
		return 0, err
	}
	if code != http.StatusOK {
		return 0, fmt.Errorf("list issues: status %d", code)
	}
	var issues []struct {
		Number  int    `json:"number"`
		Title   string `json:"title"`
		State   string `json:"state"`
		PullReq struct {
			URL string `json:"url"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(b, &issues); err != nil {
		return 0, err
	}
	for _, is := range issues {
		if is.PullReq.URL != "" {
			continue // issues endpoint can list PRs too
		}
		if strings.Contains(is.Title, "水位告警") && strings.Contains(is.Title, platform) {
			return is.Number, nil
		}
	}
	return 0, nil
}

// CloseIssue closes with a comment.
func (g *GitHubNotifier) CloseIssue(num int, comment string) error {
	if code, b, err := g.gh(http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/comments", g.Repo, num), map[string]any{"body": comment}); err != nil || code != http.StatusCreated {
		if err != nil {
			return err
		}
		return fmt.Errorf("comment: status %d: %s", code, b)
	}
	code, b, err := g.gh(http.MethodPatch, fmt.Sprintf("/repos/%s/issues/%d", g.Repo, num), map[string]any{"state": "closed"})
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("close: status %d: %s", code, b)
	}
	return nil
}

// CreateDriftIssueFull files a type:drift issue with an explicit title/body
// through the App token (AGENT_APP_SECRET env). Shared by the live canary
// driver (W7-C1) and other drift filers.
func CreateDriftIssueFull(installationID, repo, title, body string) (int, error) {
	pemRaw := os.Getenv("AGENT_APP_SECRET")
	if strings.TrimSpace(pemRaw) == "" {
		return 0, fmt.Errorf("AGENT_APP_SECRET not set — drift-issue filing disabled (fail-closed)")
	}
	key, err := parseRSAPrivateKey([]byte(pemRaw))
	if err != nil {
		return 0, err
	}
	jwtTok, err := appJWT(key)
	if err != nil {
		return 0, err
	}
	tok, err := installationToken(jwtTok, installationID)
	if err != nil {
		return 0, err
	}
	g := &GitHubNotifier{Repo: repo, InstallToken: tok, Client: &http.Client{Timeout: 30 * time.Second}}
	code, b, err := g.gh(http.MethodPost, fmt.Sprintf("/repos/%s/issues", repo), map[string]any{
		"title": title, "body": body, "labels": []string{"type:drift"},
	})
	if err != nil {
		return 0, err
	}
	if code != http.StatusCreated {
		return 0, fmt.Errorf("drift issue: status %d: %s", code, b)
	}
	var out struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.Number == 0 {
		return 0, fmt.Errorf("drift issue: bad response")
	}
	return out.Number, nil
}
