// Package vision implements the GUI-vision action layer of the P4 spec: a
// provider (OpenAI-compatible chat/completions) that turns a screenshot and
// a goal into a UI action, a no-op provider for testing, and flow scripts
// that replay distilled interaction sequences through an injected executor.
//
// Dependency rule: vision defines the adb-facing shape itself (Executor) and
// never imports internal/adb — the caller wires the device-side adapter
// (adb-backed or fake) by closure injection.
package vision

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ActionType is the vocabulary of GUI actions.
type ActionType = string

const (
	ActionTap      ActionType = "tap"       // tap absolute pixel coordinates
	ActionSwipe    ActionType = "swipe"     // swipe between two points
	ActionTypeText ActionType = "type_text" // type into the focused field
	ActionKey      ActionType = "key"       // hardware key, e.g. BACK/ENTER
	ActionUIAction ActionType = "ui_action" // pick a UI node by hint and act
	ActionDone     ActionType = "done"      // goal achieved, stop
)

// vocab is the whitelist shared by the provider (which must only emit these)
// and flow validation.
var vocab = map[ActionType]bool{
	ActionTap:      true,
	ActionSwipe:    true,
	ActionTypeText: true,
	ActionKey:      true,
	ActionUIAction: true,
	ActionDone:     true,
}

func validActionType(t ActionType) bool { return vocab[t] }

// Action is one parameterized GUI action emitted by a provider or stored in
// a flow step.
type Action struct {
	Type   ActionType     `json:"type"`
	Args   map[string]any `json:"args"`
	Reason string         `json:"reason,omitempty"`
}

// Turn is one completed provider step in the per-task session history.
type Turn struct {
	Goal     string `json:"goal"`
	Action   Action `json:"action"`
	Observed string `json:"observed,omitempty"`
}

// Provider turns a screenshot plus a goal into the next GUI action. The
// returned describe string is the human-readable rationale (it fills
// Action.Reason on the returned action); Act implementations should keep
// them in sync. history carries the turns already taken in this task.
type Provider interface {
	Act(ctx context.Context, screen []byte, goal string, history []Turn) (Action, string, error)
}

// ErrActionParse is returned when the provider reply cannot be parsed into a
// valid Action; the error message carries a fragment of the raw reply.
var ErrActionParse = errors.New("vision: parsing provider action JSON")

// OpenAICompat is an OpenAI-compatible chat/completions vision provider.
// Endpoint has the form "https://host/v1" (trailing slash tolerated); the
// request path appends "/chat/completions". APIKey may be empty for
// key-less self-hosted endpoints. Platform and AppVersion, when set, are
// injected into the (parameterized) system prompt — the template never
// encodes concrete UI coordinates.
type OpenAICompat struct {
	Endpoint   string
	APIKey     string
	Model      string
	Client     *http.Client
	Timeout    time.Duration
	Platform   string // prompt context, optional
	AppVersion string // prompt context, optional
}

// NewOpenAICompat normalizes cfg (defaults the timeout and HTTP client,
// trims the endpoint) and returns a ready provider.
func NewOpenAICompat(cfg OpenAICompat) *OpenAICompat {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 90 * time.Second
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: cfg.Timeout}
	}
	cfg.Endpoint = strings.TrimRight(cfg.Endpoint, "/")
	return &cfg
}

// Act sends the screenshot (as a base64 PNG data URL) plus goal and history
// to the chat endpoint and parses the reply into an Action. A malformed or
// out-of-vocabulary reply yields ErrActionParse with a raw text fragment.
func (p *OpenAICompat) Act(ctx context.Context, screen []byte, goal string, history []Turn) (Action, string, error) {
	if p == nil {
		return Action{}, "", errors.New("vision: nil OpenAICompat provider")
	}
	if strings.TrimSpace(goal) == "" {
		return Action{}, "", errors.New("vision: empty goal")
	}
	if p.Endpoint == "" {
		return Action{}, "", errors.New("vision: OpenAICompat.Endpoint is empty")
	}
	if p.Client == nil {
		return Action{}, "", errors.New("vision: OpenAICompat.Client is nil (use NewOpenAICompat)")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	userParts := []map[string]any{{"type": "text", "text": "Goal: " + goal}}
	if len(screen) > 0 {
		userParts = append(userParts, map[string]any{
			"type": "image_url",
			"image_url": map[string]any{
				"url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(screen),
			},
		})
	}

	messages := []map[string]any{{"role": "system", "content": p.systemPrompt()}}
	for _, t := range history {
		prevJSON, _ := json.Marshal(t.Action)
		messages = append(messages,
			map[string]any{"role": "assistant", "content": string(prevJSON)},
			map[string]any{"role": "user", "content": historyUserText(t)},
		)
	}
	messages = append(messages, map[string]any{"role": "user", "content": userParts})

	body := map[string]any{
		"messages":        messages,
		"temperature":     0,
		"response_format": map[string]any{"type": "json_object"}, // compatibility field; endpoints that ignore it still answer
	}
	if p.Model != "" {
		body["model"] = p.Model
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return Action{}, "", fmt.Errorf("vision: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.Endpoint+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return Action{}, "", fmt.Errorf("vision: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}

	resp, err := p.Client.Do(req)
	if err != nil {
		return Action{}, "", fmt.Errorf("vision: chat request: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return Action{}, "", fmt.Errorf("vision: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Action{}, "", fmt.Errorf("vision: chat endpoint status %d: %s", resp.StatusCode, truncateRunes(string(respBody), 200))
	}

	var doc struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &doc); err != nil {
		return Action{}, "", fmt.Errorf("vision: decode response: %w", err)
	}
	if len(doc.Choices) == 0 {
		return Action{}, "", errors.New("vision: chat response has no choices")
	}

	content := strings.TrimSpace(doc.Choices[0].Message.Content)
	var act Action
	if err := json.Unmarshal([]byte(content), &act); err != nil {
		return Action{}, "", fmt.Errorf("%w: %s", ErrActionParse, truncateRunes(content, 200))
	}
	if !validActionType(act.Type) {
		return Action{}, "", fmt.Errorf("%w: unknown action type %q: %s", ErrActionParse, act.Type, truncateRunes(content, 200))
	}
	describe := act.Reason
	return act, describe, nil
}

// systemPrompt returns the parameterized system prompt; platform and
// app-version come from the provider config, never hardcoded.
func (p *OpenAICompat) systemPrompt() string {
	platform := p.Platform
	if platform == "" {
		platform = "unknown"
	}
	app := p.AppVersion
	if app == "" {
		app = "unknown"
	}
	return fmt.Sprintf(systemPromptTpl, platform, app)
}

func historyUserText(t Turn) string {
	text := "Goal: " + t.Goal
	if t.Observed != "" {
		text += "\nObserved result: " + t.Observed
	}
	return text
}

const systemPromptTpl = `You are a GUI automation planner driving a mobile app.
Platform: %s. App version: %s.
Given the user's goal and the current screenshot, emit exactly one JSON action
object with three fields:
- "type": one of "tap", "swipe", "type_text", "key", "ui_action", "done"
- "args": an object with the action's parameters (fields below)
- "reason": a one-sentence justification

Action vocabulary:
- tap:        args {"x": <int px>, "y": <int px>} — tap a point on screen
- swipe:      args {"x0": <int>, "y0": <int>, "x1": <int>, "y1": <int>, "ms": <int>} — swipe from (x0,y0) to (x1,y1) over ms milliseconds
- type_text:  args {"text": "<string>"} — type text into the focused input
- key:        args {"key": "<BACK|ENTER|HOME|TAB|...>"} — press a hardware key
- ui_action:  args {"node_hint": "<short text hint of the UI node>", "click": "center"} — click the UI node identified by the hint; prefer this when the target is a labeled element
- done:       args {} — only when the goal is fully achieved and no further action is needed

Rules:
- Prefer ui_action with a descriptive node_hint for labeled elements; use tap
  only for unlabeled regions.
- Never invent coordinates for elements that are not visible in the screenshot.
- Output ONLY the JSON object. No markdown fences, no commentary.`

// NoopProvider delegates Act to Fn; it is the injection seam for tests and
// for deterministic replays.
type NoopProvider struct {
	Fn func(ctx context.Context, screen []byte, goal string, history []Turn) (Action, string, error)
}

// Act implements Provider.
func (p NoopProvider) Act(ctx context.Context, screen []byte, goal string, history []Turn) (Action, string, error) {
	if p.Fn == nil {
		return Action{}, "", errors.New("vision: NoopProvider with nil Fn")
	}
	return p.Fn(ctx, screen, goal, history)
}

// Executor is the device-side surface driven by flow steps. Callers wire an
// adb-backed adapter (adapting internal/adb to this interface); vision
// itself never imports adb.
type Executor interface {
	// Exec runs a raw device shell command and returns its combined output.
	Exec(cmd string, args ...string) (string, error)
	// Screenshot captures the current screen (PNG bytes).
	Screenshot() ([]byte, error)
	// Execute performs one GUI action and returns a human-readable result.
	Execute(a Action) (string, error)
}
