package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/an0nx/anicli-go/internal/storage"
)

// TestHistoryListPagination pins the PR82 P1#3 contract: limit/offset
// page the listing storage-side; OMITTED parameters keep the default
// FULL listing (owner constraint — existing clients see identical
// responses), and garbage values get the 422 validation contract.
func TestHistoryListPagination(t *testing.T) {
	app := newTestApp(t)
	h := app.Router()
	auth := authHeader(t, h)

	const total = 12
	for i := range total {
		poster := fmt.Sprintf("https://img.example/%d.jpg", i)
		row := &storage.AnimeProgress{
			Title:     fmt.Sprintf("Paged Anime %02d", i),
			Poster:    &poster,
			SourceID:  "fake",
			SourceURL: fmt.Sprintf("https://fake.example/paged-%d", i),
		}
		if err := app.store.Progress.Upsert(context.Background(), row); err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}

	countItems := func(payload map[string]any) int {
		items, _ := payload["items"].([]any)
		return len(items)
	}

	// Default: FULL listing (identical to the pre-pagination shape).
	_, payload := doJSON(t, h, http.MethodGet, "/api/v1/history", "", auth)
	if got := countItems(payload); got != total {
		t.Fatalf("default listing = %d items, want the full %d", got, total)
	}

	// limit alone: first window.
	_, payload = doJSON(t, h, http.MethodGet, "/api/v1/history?limit=5", "", auth)
	if got := countItems(payload); got != 5 {
		t.Fatalf("limit=5 -> %d items, want 5", got)
	}

	// limit + offset: the second window.
	_, payload = doJSON(t, h, http.MethodGet, "/api/v1/history?limit=5&offset=5", "", auth)
	items, _ := payload["items"].([]any)
	if len(items) != 5 {
		t.Fatalf("limit=5&offset=5 -> %d items, want 5", len(items))
	}
	first, _ := items[0].(map[string]any)

	// The windows must not overlap: the offset window starts where the
	// first one ended (updated_at DESC ordering).
	_, full := doJSON(t, h, http.MethodGet, "/api/v1/history", "", auth)
	fullItems, _ := full["items"].([]any)
	fifth, _ := fullItems[5].(map[string]any)
	if first["id"] != fifth["id"] {
		t.Fatalf("offset window starts at id %v, want the full listing's 6th row %v", first["id"], fifth["id"])
	}

	// offset past the end: empty, not an error.
	_, payload = doJSON(t, h, http.MethodGet, "/api/v1/history?offset=99", "", auth)
	if got := countItems(payload); got != 0 {
		t.Fatalf("offset=99 -> %d items, want 0", got)
	}

	// Garbage values: the 422 contract.
	rec, _ := doJSON(t, h, http.MethodGet, "/api/v1/history?limit=-1", "", auth)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("limit=-1 = %d, want 422", rec.Code)
	}
	rec, _ = doJSON(t, h, http.MethodGet, "/api/v1/history?limit=abc", "", auth)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("limit=abc = %d, want 422", rec.Code)
	}
}
