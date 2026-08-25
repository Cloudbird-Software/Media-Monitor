package signclient

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestSignSuccessMergesParams(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer tok-1" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"params":{"a_bogus":"sig-1","msToken":"mt-1"}}`))
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, Token: "tok-1"})
	out, err := c.Sign(t.Context(), "douyin-search", "https://x/?kw=a", map[string]string{"kw": "a"})
	if err != nil {
		t.Fatal(err)
	}
	if out["kw"] != "a" || out["a_bogus"] != "sig-1" || out["msToken"] != "mt-1" {
		t.Fatalf("params = %v", out)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestSignFailureFailClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL})
	if _, err := c.Sign(t.Context(), "c", "u", map[string]string{"a": "1"}); err == nil {
		t.Fatal("expected fail-closed error")
	}

	deg := New(Config{BaseURL: srv.URL, ReturnUnsigned: true})
	out, err := deg.Sign(t.Context(), "c", "u", map[string]string{"a": "1"})
	if err != nil || out["a"] != "1" {
		t.Fatalf("degraded = %v, %v", out, err)
	}
}

func TestWSSSignatureSigner(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"params":{"signature":"md5style"}}`))
	}))
	defer srv.Close()

	fn := New(Config{BaseURL: srv.URL}).WSSSignatureSigner("douyin-live")
	sig, err := fn("app_name=douyin_web", map[string]string{"room_id": "1"})
	if err != nil || sig != "md5style" {
		t.Fatalf("sig = %q, err = %v", sig, err)
	}
}
