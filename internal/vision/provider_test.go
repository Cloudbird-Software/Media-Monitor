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
	"net/http/httptest"
	"strings"
	"testing"
)

// pngFixture is a minimal fake screenshot payload; the provider must embed it
// as a base64 PNG data URL.
var pngFixture = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 1, 2, 3, 4, 5, 6, 7, 8}

// openAISpy is an httptest-backed fake chat/completions endpoint. It records
// the request body and Authorization header, and serves a configurable
// choices list / status.
type openAISpy struct {
	t         *testing.T
	status    int
	choices   string // raw JSON array for "choices"
	body      []byte
	auth      string
	gotMethod string
	gotPath   string
}

func newOpenAISpy(t *testing.T, choicesJSON string) *openAISpy {
	return &openAISpy{t: t, status: http.StatusOK, choices: choicesJSON}
}

func (s *openAISpy) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.gotMethod = r.Method
		s.gotPath = r.URL.Path
		s.auth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		s.body = b
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(s.status)
		if s.status != http.StatusOK {
			_, _ = io.WriteString(w, `{"error":{"message":"injected failure"}}`)
			return
		}
		fmt.Fprintf(w, `{"id":"1","object":"chat.completion","choices":%s}`, s.choices)
	})
}

// chatCompletionURL builds the endpoint form "http://host/v1".
func chatCompletionURL(ts *httptest.Server) string { return ts.URL + "/v1" }

func decodeRequestBody(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	return m
}

func messagesOf(t *testing.T, req map[string]any) []map[string]any {
	t.Helper()
	raw, err := json.Marshal(req["messages"])
	if err != nil {
		t.Fatalf("marshal messages: %v", err)
	}
	var msgs []map[string]any
	if err := json.Unmarshal(raw, &msgs); err != nil {
		t.Fatalf("decode messages: %v", err)
	}
	return msgs
}

// TestActParsesActionAndSendsImage: the request carries history, the goal,
// the base64 PNG data URL, response_format and temperature 0; the reply is
// parsed into an Action whose Reason matches the returned describe.
func TestActParsesActionAndSendsImage(t *testing.T) {
	spy := newOpenAISpy(t, `[{"index":0,"message":{"role":"assistant","content":"{\"type\":\"tap\",\"args\":{\"x\":100,\"y\":200},\"reason\":\"hit the comment button\"}"},"finish_reason":"stop"}]`)
	srv := httptest.NewServer(spy.Handler())
	defer srv.Close()
	p := NewOpenAICompat(OpenAICompat{
		Endpoint: chatCompletionURL(srv),
		APIKey:   "sk-secret",
		Model:    "vm-model",
		Platform: "douyin",
		Client:   srv.Client(),
	})

	log := []Turn{
		{
			Goal:     "open comments",
			Action:   Action{Type: ActionTap, Args: map[string]any{"x": 1, "y": 2}},
			Observed: "nothing happened",
		},
	}
	act, describe, err := p.Act(context.Background(), pngFixture, "open comments", log)
	if err != nil {
		t.Fatalf("Act: %v", err)
	}
	if act.Type != ActionTap {
		t.Fatalf("type = %q, want %q", act.Type, ActionTap)
	}
	if act.Args["x"] != float64(100) || act.Args["y"] != float64(200) {
		t.Fatalf("args = %v", act.Args)
	}
	if describe != "hit the comment button" || act.Reason != describe {
		t.Fatalf("describe = %q, action.Reason = %q; describe must fill Reason", describe, act.Reason)
	}

	if spy.gotMethod != http.MethodPost || spy.gotPath != "/v1/chat/completions" {
		t.Fatalf("request = %s %s", spy.gotMethod, spy.gotPath)
	}
	if spy.auth != "Bearer sk-secret" {
		t.Fatalf("Authorization = %q", spy.auth)
	}
	req := decodeRequestBody(t, spy.body)
	if req["model"] != "vm-model" {
		t.Fatalf("model = %v", req["model"])
	}
	if req["temperature"] != float64(0) {
		t.Fatalf("temperature = %v", req["temperature"])
	}
	rf, ok := req["response_format"].(map[string]any)
	if !ok || rf["type"] != "json_object" {
		t.Fatalf("response_format = %v", req["response_format"])
	}

	msgs := messagesOf(t, req)
	if len(msgs) != 4 {
		t.Fatalf("messages = %d, want 4 (system + history pair + current user)", len(msgs))
	}
	if msgs[0]["role"] != "system" {
		t.Fatalf("first message role = %v", msgs[0]["role"])
	}
	if !strings.Contains(msgs[0]["content"].(string), "douyin") {
		t.Fatal("system prompt must be parameterized with the platform name")
	}
	if msgs[1]["role"] != "assistant" || !strings.Contains(msgs[1]["content"].(string), `"type":"tap"`) {
		t.Fatalf("history assistant message = %v", msgs[1])
	}
	if msgs[2]["role"] != "user" || !strings.Contains(msgs[2]["content"].(string), "Observed result: nothing happened") {
		t.Fatalf("history user message = %v", msgs[2])
	}

	// current user message: text part + image part
	userContent, ok := msgs[3]["content"].([]any)
	if !ok {
		t.Fatalf("current user content type = %T", msgs[3]["content"])
	}
	if len(userContent) != 2 {
		t.Fatalf("user content parts = %d, want 2 (text + image)", len(userContent))
	}
	imgPart, ok := userContent[1].(map[string]any)
	if !ok || imgPart["type"] != "image_url" {
		t.Fatalf("image part = %v", userContent[1])
	}
	imageInfo, _ := imgPart["image_url"].(map[string]any)
	url, _ := imageInfo["url"].(string)
	wantURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngFixture)
	if url != wantURL {
		t.Fatalf("image url = %q, want %q", url, wantURL)
	}
	if !bytes.Contains(spy.body, []byte("data:image/png;base64,")) {
		t.Fatal("request body must contain the base64 data URL")
	}
}

// TestActWithoutScreen: with no screenshot the user message has no image part.
func TestActWithoutScreen(t *testing.T) {
	spy := newOpenAISpy(t, `[{"index":0,"message":{"role":"assistant","content":"{\"type\":\"done\",\"args\":{}}"}}]`)
	srv := httptest.NewServer(spy.Handler())
	defer srv.Close()
	p := NewOpenAICompat(OpenAICompat{Endpoint: srv.URL, Client: srv.Client()})

	act, _, err := p.Act(context.Background(), nil, "just stop", nil)
	if err != nil {
		t.Fatalf("Act: %v", err)
	}
	if act.Type != ActionDone {
		t.Fatalf("type = %q, want done", act.Type)
	}
	if bytes.Contains(spy.body, []byte("data:image")) {
		t.Fatal("request must not carry an image when screen is empty")
	}
	msgs := messagesOf(t, decodeRequestBody(t, spy.body))
	userContent, ok := msgs[len(msgs)-1]["content"].([]any)
	if !ok || len(userContent) != 1 {
		t.Fatalf("user content = %v, want exactly one text part", msgs[len(msgs)-1]["content"])
	}
}

// TestActBadJSON: an unparseable reply yields ErrActionParse carrying a raw
// text fragment; an out-of-vocabulary type is equally an action parse error.
func TestActBadJSON(t *testing.T) {
	cases := map[string]string{
		"broken JSON":    "{\"type\":\"ta",
		"unknown action": "{\"type\":\"fly\",\"args\":{}}",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			spy := newOpenAISpy(t, `[{"index":0,"message":{"role":"assistant","content":`+jsonContent(content)+`}}]`)
			srv := httptest.NewServer(spy.Handler())
			defer srv.Close()
			p := NewOpenAICompat(OpenAICompat{Endpoint: srv.URL, Client: srv.Client()})

			_, _, err := p.Act(context.Background(), pngFixture, "goal", nil)
			if !errors.Is(err, ErrActionParse) {
				t.Fatalf("err = %v, want ErrActionParse", err)
			}
			if !strings.Contains(err.Error(), content) {
				t.Fatalf("error %q must carry a fragment of the raw reply %q", err, content)
			}
		})
	}
}

// jsonContent quotes s as a JSON string for embedding.
func jsonContent(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestActServerError: non-200 statuses surface the status and a body
// fragment.
func TestActServerError(t *testing.T) {
	spy := newOpenAISpy(t, `[]`)
	spy.status = http.StatusInternalServerError
	srv := httptest.NewServer(spy.Handler())
	defer srv.Close()
	p := NewOpenAICompat(OpenAICompat{Endpoint: srv.URL, Client: srv.Client()})

	_, _, err := p.Act(context.Background(), pngFixture, "goal", nil)
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v, want status 500 error", err)
	}
}

// TestActNoChoices: an empty choices array is an explicit error.
func TestActNoChoices(t *testing.T) {
	spy := newOpenAISpy(t, `[]`)
	srv := httptest.NewServer(spy.Handler())
	defer srv.Close()
	p := NewOpenAICompat(OpenAICompat{Endpoint: srv.URL, Client: srv.Client()})

	if _, _, err := p.Act(context.Background(), pngFixture, "goal", nil); err == nil {
		t.Fatal("expected no-choices error")
	}
}

// TestActValidation: empty goal and missing endpoint are rejected before any
// request is sent.
func TestActValidation(t *testing.T) {
	p := NewOpenAICompat(OpenAICompat{Client: &http.Client{}})
	if _, _, err := p.Act(context.Background(), nil, "  ", nil); err == nil {
		t.Fatal("empty goal must error")
	}
	if _, _, err := p.Act(context.Background(), nil, "goal", nil); err == nil {
		t.Fatal("empty endpoint must error")
	}
}

// TestNoopProvider: Fn receives the call and its result is passed through;
// a nil Fn is an explicit error.
func TestNoopProvider(t *testing.T) {
	var gotGoal string
	var gotHistory []Turn
	np := NoopProvider{Fn: func(ctx context.Context, screen []byte, goal string, history []Turn) (Action, string, error) {
		gotGoal = goal
		gotHistory = history
		return Action{Type: ActionKey, Args: map[string]any{"key": "BACK"}}, "go back", nil
	}}
	hist := []Turn{{Goal: "g", Action: Action{Type: ActionTap}, Observed: "o"}}
	act, desc, err := np.Act(context.Background(), nil, "press back", hist)
	if err != nil {
		t.Fatalf("Act: %v", err)
	}
	if gotGoal != "press back" || len(gotHistory) != 1 || gotHistory[0].Goal != "g" {
		t.Fatalf("Fn captured goal=%q history=%v", gotGoal, gotHistory)
	}
	if act.Type != ActionKey || act.Args["key"] != "BACK" || desc != "go back" {
		t.Fatalf("Act result = %+v, %q", act, desc)
	}

	empty := NoopProvider{}
	if _, _, err := empty.Act(context.Background(), nil, "g", nil); err == nil {
		t.Fatal("nil Fn must error")
	}
}
