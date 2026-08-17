// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gke-demos/hallway/embed"
)

// ---- fixtures ----

// countingEmbedder records every text it is asked to embed, so tests can assert
// that a refresh only pays for what actually changed.
type countingEmbedder struct {
	mu       sync.Mutex
	embedded []string
	calls    int
}

func (c *countingEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.embedded = append(c.embedded, texts...)
	out := make([][]float32, len(texts))
	for i, t := range texts {
		// Deterministic pseudo-vector; content-dependent so distinct docs differ.
		out[i] = []float32{float32(len(t)), float32(strings.Count(t, "e")), 1}
	}
	return out, nil
}

func (c *countingEmbedder) EmbedQuery(_ context.Context, text string) ([]float32, error) {
	return []float32{float32(len(text)), float32(strings.Count(text, "e")), 1}, nil
}

func (c *countingEmbedder) Name() string { return "counting-v1" }

func (c *countingEmbedder) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.embedded)
}

// sessionizePayload builds an /All body with n sessions plus a /Speakers body.
func sessionizePayload(n int) (allJSON, speakersJSON []byte) {
	type sess struct {
		ID          string   `json:"id"`
		Title       string   `json:"title"`
		Description string   `json:"description"`
		StartsAt    string   `json:"startsAt"`
		EndsAt      string   `json:"endsAt"`
		Speakers    []string `json:"speakers"`
		RoomID      int      `json:"roomId"`
	}
	var sessions []sess
	for i := 0; i < n; i++ {
		sessions = append(sessions, sess{
			ID:          fmt.Sprintf("s%d", i),
			Title:       fmt.Sprintf("Session %d", i),
			Description: fmt.Sprintf("About topic number %d", i),
			StartsAt:    fmt.Sprintf("2027-03-23T%02d:00:00", 9+i%8),
			EndsAt:      fmt.Sprintf("2027-03-23T%02d:30:00", 9+i%8),
			Speakers:    []string{"sp1"},
			RoomID:      1,
		})
	}
	allJSON, _ = json.Marshal(map[string]any{
		"sessions": sessions,
		"rooms":    []map[string]any{{"id": 1, "name": "Hall A"}},
	})
	speakersJSON, _ = json.Marshal([]map[string]any{{
		"id":       "sp1",
		"fullName": "Ada Lovelace",
		"questionAnswers": []map[string]string{
			{"question": "Company", "answer": "Analytical Engines"},
		},
	}})
	return allJSON, speakersJSON
}

// fakeSessionize serves the two views, with a swappable session count and an
// optional forced failure.
type fakeSessionize struct {
	*httptest.Server
	sessions atomic.Int64
	fail     atomic.Bool
	hits     atomic.Int64
}

func newFakeSessionize(t *testing.T, sessions int) *fakeSessionize {
	t.Helper()
	f := &fakeSessionize{}
	f.sessions.Store(int64(sessions))
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		if f.fail.Load() {
			http.Error(w, "upstream exploded", http.StatusInternalServerError)
			return
		}
		all, spk := sessionizePayload(int(f.sessions.Load()))
		switch {
		case strings.HasSuffix(r.URL.Path, "/All"):
			w.Write(all)
		case strings.HasSuffix(r.URL.Path, "/Speakers"):
			w.Write(spk)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

// registerTestEvent puts fakeEvent in the registry for the duration of a test.
func registerTestEvent(t *testing.T) Event {
	t.Helper()
	if err := RegisterEvent(fakeEvent); err != nil {
		t.Fatalf("RegisterEvent: %v", err)
	}
	t.Cleanup(func() {
		eventsMu.Lock()
		delete(events, fakeEvent.Slug)
		eventsMu.Unlock()
	})
	return fakeEvent
}

func newTestStore(t *testing.T, srvURL string, emb *countingEmbedder) *Store {
	t.Helper()
	dir := t.TempDir()
	return NewStore(StoreConfig{
		Dir:           dir,
		CacheDir:      dir,
		SessionizeURL: srvURL,
		Embedder:      emb,
		Logf:          func(format string, args ...any) { t.Logf(format, args...) },
	})
}

// ---- tests ----

// A store with no local files fetches on first Get, and reuses the snapshot
// afterwards without hitting the network again.
func TestStoreLoadsOnDemandThenCaches(t *testing.T) {
	registerTestEvent(t)
	srv := newFakeSessionize(t, 3)
	store := newTestStore(t, srv.URL, &countingEmbedder{})
	ctx := context.Background()

	snap, err := store.Get(ctx, fakeEvent.Slug)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := len(snap.Index.SessionList); got != 3 {
		t.Errorf("sessions = %d, want 3", got)
	}
	if snap.Source != "sessionize" {
		t.Errorf("source = %q, want sessionize", snap.Source)
	}
	afterFirst := srv.hits.Load()

	// Second Get must be served from memory.
	snap2, err := store.Get(ctx, fakeEvent.Slug)
	if err != nil {
		t.Fatalf("Get again: %v", err)
	}
	if snap2 != snap {
		t.Errorf("second Get returned a different snapshot; expected the cached one")
	}
	if srv.hits.Load() != afterFirst {
		t.Errorf("second Get hit the network (%d -> %d)", afterFirst, srv.hits.Load())
	}

	// The fetch should have been persisted under the slug-derived names.
	allPath, spkPath := store.Paths(fakeEvent.Slug)
	if !fileNonEmpty(allPath) || !fileNonEmpty(spkPath) {
		t.Errorf("expected fetched data cached to %s / %s", allPath, spkPath)
	}
	if want := filepath.Base(allPath); want != fakeEvent.Slug+"-all.json" {
		t.Errorf("data file = %q, want %s-all.json", want, fakeEvent.Slug)
	}
}

// Refresh swaps in new data; the previously returned snapshot keeps working.
func TestStoreRefreshSwapsSnapshot(t *testing.T) {
	registerTestEvent(t)
	srv := newFakeSessionize(t, 3)
	store := newTestStore(t, srv.URL, &countingEmbedder{})
	ctx := context.Background()

	old, err := store.Get(ctx, fakeEvent.Slug)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	srv.sessions.Store(5)
	fresh, err := store.Refresh(ctx, fakeEvent.Slug)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := len(fresh.Index.SessionList); got != 5 {
		t.Errorf("refreshed sessions = %d, want 5", got)
	}
	if got := len(old.Index.SessionList); got != 3 {
		t.Errorf("old snapshot mutated: %d sessions, want 3 — snapshots must be immutable", got)
	}
	if store.Peek(fakeEvent.Slug) != fresh {
		t.Errorf("Peek did not return the refreshed snapshot")
	}
}

// A failed refresh must leave the live snapshot in place and surface the error.
func TestStoreFailedRefreshKeepsOldData(t *testing.T) {
	registerTestEvent(t)
	srv := newFakeSessionize(t, 4)
	store := newTestStore(t, srv.URL, &countingEmbedder{})
	ctx := context.Background()

	good, err := store.Get(ctx, fakeEvent.Slug)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	srv.fail.Store(true)
	if _, err := store.Refresh(ctx, fakeEvent.Slug); err == nil {
		t.Fatal("expected refresh to fail")
	}
	if store.Peek(fakeEvent.Slug) != good {
		t.Errorf("failed refresh replaced the live snapshot")
	}
	if got := len(store.Peek(fakeEvent.Slug).Index.SessionList); got != 4 {
		t.Errorf("sessions = %d, want the original 4", got)
	}
}

// Concurrent first-touches of the same event must build once, not N times.
func TestStoreConcurrentGetBuildsOnce(t *testing.T) {
	registerTestEvent(t)
	srv := newFakeSessionize(t, 6)
	emb := &countingEmbedder{}
	store := newTestStore(t, srv.URL, emb)
	ctx := context.Background()

	const n = 8
	var wg sync.WaitGroup
	snaps := make([]*Snapshot, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			snaps[i], errs[i] = store.Get(ctx, fakeEvent.Slug)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	for i := 1; i < n; i++ {
		if snaps[i] != snaps[0] {
			t.Errorf("goroutine %d got a different snapshot; expected a single shared build", i)
		}
	}
	// Two view fetches for one build. More means the singleflight lock leaked.
	if hits := srv.hits.Load(); hits != 2 {
		t.Errorf("upstream hits = %d, want 2 (one build)", hits)
	}
	if emb.calls != 1 {
		t.Errorf("embedder calls = %d, want 1", emb.calls)
	}
}

// The whole point of the per-document cache: adding sessions embeds only the
// new ones, not the entire corpus.
func TestRefreshEmbedsOnlyNewSessions(t *testing.T) {
	registerTestEvent(t)
	srv := newFakeSessionize(t, 10)
	emb := &countingEmbedder{}
	store := newTestStore(t, srv.URL, emb)
	ctx := context.Background()

	if _, err := store.Get(ctx, fakeEvent.Slug); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := emb.total(); got != 10 {
		t.Fatalf("initial embed count = %d, want 10", got)
	}

	// Two new sessions appear; the other ten are byte-identical.
	srv.sessions.Store(12)
	if _, err := store.Refresh(ctx, fakeEvent.Slug); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := emb.total(); got != 12 {
		t.Errorf("total embedded = %d, want 12 (10 cached + 2 new); "+
			"a whole-corpus cache key would give 22", got)
	}

	// A refresh with no changes must embed nothing at all.
	before := emb.total()
	if _, err := store.Refresh(ctx, fakeEvent.Slug); err != nil {
		t.Fatalf("Refresh (no-op): %v", err)
	}
	if got := emb.total(); got != before {
		t.Errorf("unchanged refresh embedded %d new documents, want 0", got-before)
	}
}

// Switching embedders must invalidate the cache rather than mixing vector spaces.
type otherEmbedder struct{ countingEmbedder }

func (o *otherEmbedder) Name() string { return "other-v2" }

func TestVectorCacheInvalidatesOnEmbedderChange(t *testing.T) {
	registerTestEvent(t)
	srv := newFakeSessionize(t, 5)
	dir := t.TempDir()

	build := func(emb embed.Embedder) {
		store := NewStore(StoreConfig{
			Dir: dir, CacheDir: dir, SessionizeURL: srv.URL, Embedder: emb,
			Logf: func(f string, a ...any) { t.Logf(f, a...) },
		})
		if _, err := store.Get(context.Background(), fakeEvent.Slug); err != nil {
			t.Fatalf("Get: %v", err)
		}
	}

	first := &countingEmbedder{}
	build(first)
	if first.total() != 5 {
		t.Fatalf("first embedder embedded %d, want 5", first.total())
	}

	second := &otherEmbedder{}
	build(second)
	if second.total() != 5 {
		t.Errorf("after switching embedders %d documents were embedded, want all 5 "+
			"(cached vectors from another model must not be reused)", second.total())
	}
}

// Auto-refresh picks up upstream changes without a restart.
func TestRefreshLoadedUpdatesEveryLoadedEvent(t *testing.T) {
	registerTestEvent(t)
	srv := newFakeSessionize(t, 2)
	store := newTestStore(t, srv.URL, &countingEmbedder{})
	ctx := context.Background()

	if _, err := store.Get(ctx, fakeEvent.Slug); err != nil {
		t.Fatalf("Get: %v", err)
	}
	srv.sessions.Store(7)
	store.RefreshLoaded(ctx)

	if got := len(store.Peek(fakeEvent.Slug).Index.SessionList); got != 7 {
		t.Errorf("after RefreshLoaded sessions = %d, want 7", got)
	}
}

// A snapshot built from disk is stamped with the file's mtime, not "now".
func TestLocalSnapshotSourceAndStamp(t *testing.T) {
	registerTestEvent(t)
	srv := newFakeSessionize(t, 3)
	store := newTestStore(t, srv.URL, &countingEmbedder{})
	ctx := context.Background()

	// First Get fetches and writes the files.
	if _, err := store.Get(ctx, fakeEvent.Slug); err != nil {
		t.Fatalf("Get: %v", err)
	}
	allPath, spkPath := store.Paths(fakeEvent.Slug)

	// A fresh store over the same directory should read locally, no network.
	hitsBefore := srv.hits.Load()
	store2 := NewStore(StoreConfig{
		Dir: filepath.Dir(allPath), CacheDir: filepath.Dir(spkPath),
		SessionizeURL: srv.URL, Logf: func(f string, a ...any) { t.Logf(f, a...) },
	})
	snap, err := store2.Get(ctx, fakeEvent.Slug)
	if err != nil {
		t.Fatalf("Get from local: %v", err)
	}
	if snap.Source != "local" {
		t.Errorf("source = %q, want local", snap.Source)
	}
	if srv.hits.Load() != hitsBefore {
		t.Errorf("local load hit the network")
	}
	if snap.FetchedAt.IsZero() || time.Since(snap.FetchedAt) > time.Hour {
		t.Errorf("FetchedAt = %v, want the data file's mtime", snap.FetchedAt)
	}
}

// An unknown slug is a caller error, reported with the known slugs.
func TestStoreUnknownEvent(t *testing.T) {
	store := newTestStore(t, "http://127.0.0.1:0", &countingEmbedder{})
	_, err := store.Get(context.Background(), "not-a-conference")
	if err == nil {
		t.Fatal("expected an error for an unknown event")
	}
	if !strings.Contains(err.Error(), "unknown event") {
		t.Errorf("error = %q, want it to mention 'unknown event'", err)
	}
}
