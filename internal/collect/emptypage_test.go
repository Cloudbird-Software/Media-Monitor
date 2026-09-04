package collect

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Cloudbird-Software/Media-Monitor/internal/contracts"
	"github.com/Cloudbird-Software/Media-Monitor/internal/model"
	"github.com/Cloudbird-Software/Media-Monitor/internal/platforms/douyin"
)

// TestCheckBindingsNullIsCleanEmpty: a JSON null primary binding (douyin's
// zero-comment item shape, {"comments": null}) is a VALID zero-record page —
// not an ErrEmptyPage. ErrEmptyPage is the auto-mode rotation trigger, so the
// old behavior burned accounts on genuinely comment-less items (report G3).
func TestCheckBindingsNullIsCleanEmpty(t *testing.T) {
	c := &contracts.Contract{Binding: contracts.Binding{Comments: "$.comments"}}
	if err := checkBindings(c, map[string]any{"comments": nil}); err != nil {
		t.Fatalf("comments:null must be a clean empty page, got %v", err)
	}
	if err := checkBindings(c, map[string]any{"comments": []any{}}); err != nil {
		t.Fatalf("comments:[] must be a clean empty page, got %v", err)
	}
	// Missing path and non-list shapes stay fail-closed.
	if err := checkBindings(c, map[string]any{"other": 1}); err == nil {
		t.Fatal("missing binding path must fail closed")
	}
	if err := checkBindings(c, map[string]any{"comments": map[string]any{}}); err == nil {
		t.Fatal("non-list binding must fail closed")
	}
}

// TestItemCommentsNullPageCleanWalk: end-to-end shape — the dy comments
// contract against a mock returning {"comments":null, "has_more":0} yields
// zero comments, a nil error and exactly ONE request (no retry storm, no
// rotation eligibility).
func TestItemCommentsNullPageCleanWalk(t *testing.T) {
	var reqs int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&reqs, 1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status_code":0,"comments":null,"has_more":0,"cursor":0,"total":0}`)
	}))
	defer srv.Close()
	reg := contracts.NewRegistry()
	reg.Add(&contracts.Contract{
		Name: "douyin-comments", Platform: douyin.Platform, Category: "comments", Version: "1",
		Transport: contracts.Transport{BaseURL: srv.URL, Path: "/aweme/v1/web/comment/list/", Method: http.MethodGet, Placeholders: []string{"aweme_id"}},
		Binding:   contracts.Binding{Comments: "$.comments"},
		Paging:    contracts.Paging{HasMorePath: "$.has_more"},
	})
	e := New(Context{Registry: reg, Names: map[string]map[string]string{douyin.Platform: {"comments": "douyin-comments"}}})
	cmts, _, err := e.ItemComments(context.Background(), douyin.Platform, "7000000000000000000", model.Cursor{}, 20)
	if err != nil {
		t.Fatalf("ItemComments on a null-comment item must be a clean empty success, got %v", err)
	}
	if len(cmts) != 0 {
		t.Fatalf("expected 0 comments, got %d", len(cmts))
	}
	if n := atomic.LoadInt32(&reqs); n != 1 {
		t.Fatalf("expected exactly 1 request (no retries/rotation), got %d", n)
	}
}
