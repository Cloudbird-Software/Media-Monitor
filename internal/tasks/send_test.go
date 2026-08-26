package tasks

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Cloudbird-Software/Media-Monitor/internal/collect"
	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/httpclient"
	"github.com/Cloudbird-Software/Media-Monitor/internal/obs"
	"github.com/Cloudbird-Software/Media-Monitor/internal/store"
)

// fakeSendServer answers send-message POSTs. failFirst, when >0, makes the
// first N attempts fail (to exercise the once-only retry).
func fakeSendServer(t *testing.T, failFirst *atomic.Int64, bodies *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &m)
		out, _ := json.Marshal(m)
		*bodies = append(*bodies, string(out))
		if failFirst != nil && failFirst.Add(-1) >= 0 {
			// A non-2xx status makes the engine surface a transport error, which
			// is what triggers the once-only retry in the Sender.
			http.Error(w, `{"status_code":1}`, http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"data":{"status":"sent","msg_id":"m-1"},"status_code":0}`))
	}))
}

func newSendEngine(t *testing.T, srv *httptest.Server) *collect.Engine {
	t.Helper()
	c := &contracts.Contract{
		Name:     "douyin-send-message",
		Platform: "douyin",
		Category: "send_message",
		Version:  "1",
		Transport: contracts.Transport{
			BaseURL: srv.URL, Path: "/v1/message/send/", Method: "POST",
			Query: map[string]string{"aid": "6383"},
			Body:  map[string]any{"sec_user_id": "", "text": ""},
		},
		Signature: contracts.Signature{Params: []string{"a_bogus"}, Required: []string{"a_bogus"}},
		Binding:   contracts.Binding{Fields: map[string]string{"status": "$.data.status"}},
	}
	reg := contracts.NewRegistry()
	if err := reg.Add(c); err != nil {
		t.Fatal(err)
	}
	return collect.New(collect.Context{
		Registry: reg,
		HTTP:     httpclient.New(httpclient.Config{Timeout: 3 * time.Second, UserAgents: []string{"t"}}),
		Obs:      obs.NewCounterMap(),
		Signers:  map[string]httpclient.Signer{"douyin": signerStub{}},
		Names:    map[string]map[string]string{"douyin": {"send_message": "douyin-send-message"}},
	})
}

type signerStub struct{}

func (signerStub) Sign(_ context.Context, contractName, _ string, _ map[string]string) (map[string]string, error) {
	return map[string]string{"a_bogus": "ab-" + contractName}, nil
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestSendFirstAndSecondMessage(t *testing.T) {
	var bodies []string
	srv := fakeSendServer(t, nil, &bodies)
	defer srv.Close()
	s := NewSender(newSendEngine(t, srv), nil)
	// Injected fake clock: records delays, never sleeps for real (the 9s
	// configured delay below would otherwise dominate the test runtime).
	var delays []time.Duration
	s.SetSleep(func(_ context.Context, d time.Duration) error {
		delays = append(delays, d)
		return nil
	})

	rep, err := s.Run(context.Background(), SendTaskConfig{
		Platform:       "douyin",
		Targets:        []string{"sec-1"},
		FirstMessage:   MessageTemplate{Content: "你好 {nickname}"},
		SecondMessage:  &MessageTemplate{Content: "这是第二条"},
		SecondDelayMs:  9000,
		SubstituteNick: map[string]string{"sec-1": "张三"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Results) != 1 {
		t.Fatalf("results = %d", len(rep.Results))
	}
	if rep.Results[0].FirstStatus != "sent" || rep.Results[0].SecondStatus != "sent" {
		t.Fatalf("statuses = %+v", rep.Results[0])
	}
	if len(bodies) != 2 {
		t.Fatalf("bodies = %d, want 2", len(bodies))
	}
	if !strings.Contains(bodies[0], `"text":"你好 张三"`) {
		t.Fatalf("nickname not substituted: %s", bodies[0])
	}
	if !strings.Contains(bodies[0], `"sec_user_id":"sec-1"`) {
		t.Fatalf("sec_user_id missing: %s", bodies[0])
	}
	if !strings.Contains(bodies[1], `"text":"这是第二条"`) {
		t.Fatalf("second message wrong: %s", bodies[1])
	}
	if len(delays) != 1 || delays[0] != 9*time.Second {
		t.Fatalf("delays = %v, want [9s] (injected clock)", delays)
	}
}

func TestSendRetryOnceThenGiveUp(t *testing.T) {
	var failFirst atomic.Int64
	failFirst.Store(2) // both attempts for the single target fail
	var bodies []string
	srv := fakeSendServer(t, &failFirst, &bodies)
	defer srv.Close()
	s := NewSender(newSendEngine(t, srv), nil)

	rep, err := s.Run(context.Background(), SendTaskConfig{
		Platform:     "douyin",
		Targets:      []string{"sec-x"},
		FirstMessage: MessageTemplate{Content: "hi"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Results) != 1 || rep.Results[0].Error == "" {
		t.Fatalf("expected an error result, got %+v", rep.Results[0])
	}
	if len(bodies) != 2 { // initial + one retry
		t.Fatalf("bodies = %d, want 2 (one retry)", len(bodies))
	}
}

func TestSendRetryRecovers(t *testing.T) {
	var failFirst atomic.Int64
	failFirst.Store(1) // only the first attempt fails
	var bodies []string
	srv := fakeSendServer(t, &failFirst, &bodies)
	defer srv.Close()
	s := NewSender(newSendEngine(t, srv), nil)

	rep, err := s.Run(context.Background(), SendTaskConfig{
		Platform:     "douyin",
		Targets:      []string{"sec-y"},
		FirstMessage: MessageTemplate{Content: "hi"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Results[0].FirstStatus != "sent" || rep.Results[0].Error != "" {
		t.Fatalf("retry should have recovered: %+v", rep.Results[0])
	}
	if len(bodies) != 2 {
		t.Fatalf("bodies = %d, want 2", len(bodies))
	}
}

func TestSendCapSkipsExceeded(t *testing.T) {
	var bodies []string
	srv := fakeSendServer(t, nil, &bodies)
	defer srv.Close()
	s := NewSender(newSendEngine(t, srv), newStore(t))

	for i := 0; i < 2; i++ {
		s.bumpCount("acc-1", "douyin")
	}
	rep, err := s.Run(context.Background(), SendTaskConfig{
		Platform:      "douyin",
		AccountID:     "acc-1",
		SendCap:       2,
		Targets:       []string{"s1", "s2", "s3"},
		FirstMessage:  MessageTemplate{Content: "hi"},
		SecondMessage: &MessageTemplate{Content: "second"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Skipped != 3 {
		t.Fatalf("skipped = %d, want 3 (cap already reached)", rep.Skipped)
	}
	if len(bodies) != 0 {
		t.Fatalf("bodies = %d, want 0", len(bodies))
	}
}

func TestSendCapAllowsUpToCap(t *testing.T) {
	var bodies []string
	srv := fakeSendServer(t, nil, &bodies)
	defer srv.Close()
	s := NewSender(newSendEngine(t, srv), newStore(t))
	var delays []time.Duration
	s.SetSleep(func(_ context.Context, d time.Duration) error {
		delays = append(delays, d)
		return nil
	})

	rep, err := s.Run(context.Background(), SendTaskConfig{
		Platform:      "douyin",
		AccountID:     "acc-2",
		SendCap:       3,
		Targets:       []string{"a", "b", "c", "d", "e"},
		FirstMessage:  MessageTemplate{Content: "first"},
		SecondMessage: &MessageTemplate{Content: "second"},
		SecondDelayMs: 5000,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 3 sends allowed: target a (first+second=2) + target b (first=1) = 3, then capped.
	if len(bodies) != 3 {
		t.Fatalf("bodies = %d, want 3 (cap=3 sends)", len(bodies))
	}
	if rep.Skipped != 3 {
		t.Fatalf("skipped = %d, want 3 (d,e + remainder)", rep.Skipped)
	}
	// Only target a got its second message: exactly one 5s (fake) delay.
	if len(delays) != 1 || delays[0] != 5*time.Second {
		t.Fatalf("delays = %v, want [5s]", delays)
	}
}

func TestSendRequiresPlatformAndTargets(t *testing.T) {
	s := NewSender(newSendEngine(t, httptest.NewServer(nil)), nil)
	if _, err := s.Run(context.Background(), SendTaskConfig{}); err == nil {
		t.Fatal("expected error for empty config")
	}
	if _, err := s.Run(context.Background(), SendTaskConfig{Platform: "douyin"}); err == nil {
		t.Fatal("expected error for empty targets")
	}
}
