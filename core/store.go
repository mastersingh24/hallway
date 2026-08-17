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
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/gke-demos/hallway/embed"
)

// Snapshot is an immutable, fully-built view of one event's data: the joined
// index plus the search structures over it, and a note of when the underlying
// payload was obtained.
//
// Snapshots are never mutated. A refresh builds an entirely new one alongside
// the live one and swaps it in only on success, so in-flight tool calls keep
// serving consistent data and a failed refresh changes nothing.
type Snapshot struct {
	Event     Event
	Index     *Index
	Searcher  *Searcher
	FetchedAt time.Time
	Source    string // "sessionize" (fetched) or "local" (read from disk)
}

// Age reports how long ago this snapshot's data was obtained.
func (s *Snapshot) Age() time.Duration { return time.Since(s.FetchedAt) }

// StoreConfig describes where event data comes from and where derived
// artifacts are kept.
type StoreConfig struct {
	// Dir holds per-event data files, named <slug>-all.json and
	// <slug>-speakers.json.
	Dir string

	// CacheDir holds per-event embedding caches, named <slug>.embeddings.json.
	// Empty disables the on-disk cache.
	CacheDir string

	// SessionizeURL is the API base used when fetching. Empty means the default.
	SessionizeURL string

	// Embedder enables semantic search. Nil means keyword-only.
	Embedder embed.Embedder

	// Primary is the event selected at startup. PrimaryAll and PrimarySpeakers,
	// when set, override the derived paths for that one event — this is what
	// keeps the plain `-all all.json -speakers speakers.json` invocation working
	// now that other events use slug-prefixed names.
	Primary         string
	PrimaryAll      string
	PrimarySpeakers string

	// Logf receives operational messages. Nil uses the standard logger.
	Logf func(format string, args ...any)
}

// Store owns the loaded snapshots, one per event, and serializes the work of
// building them. It is safe for concurrent use: reads take an RLock and see a
// consistent snapshot pointer; builds happen off to the side.
type Store struct {
	cfg StoreConfig

	mu    sync.RWMutex
	snaps map[string]*Snapshot

	// buildMu guards builders; builders[slug] serializes concurrent builds of
	// the same event so two tool calls racing on an unloaded event don't both
	// fetch and both pay to embed.
	buildMu  sync.Mutex
	builders map[string]*sync.Mutex
}

// NewStore returns an empty store; events load on first use.
func NewStore(cfg StoreConfig) *Store {
	return &Store{
		cfg:      cfg,
		snaps:    map[string]*Snapshot{},
		builders: map[string]*sync.Mutex{},
	}
}

func (s *Store) logf(format string, args ...any) {
	if s.cfg.Logf != nil {
		s.cfg.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// Paths returns the data file paths for an event slug.
func (s *Store) Paths(slug string) (allPath, speakersPath string) {
	if slug == s.cfg.Primary && s.cfg.PrimaryAll != "" && s.cfg.PrimarySpeakers != "" {
		return s.cfg.PrimaryAll, s.cfg.PrimarySpeakers
	}
	return filepath.Join(s.cfg.Dir, slug+"-all.json"),
		filepath.Join(s.cfg.Dir, slug+"-speakers.json")
}

func (s *Store) cachePath(slug string) string {
	if s.cfg.CacheDir == "" {
		return ""
	}
	return filepath.Join(s.cfg.CacheDir, slug+".embeddings.json")
}

// Peek returns the currently loaded snapshot for a slug, or nil.
func (s *Store) Peek(slug string) *Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snaps[slug]
}

// Loaded returns every loaded snapshot, ordered by slug.
func (s *Store) Loaded() []*Snapshot {
	s.mu.RLock()
	out := make([]*Snapshot, 0, len(s.snaps))
	for _, snap := range s.snaps {
		out = append(out, snap)
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Event.Slug < out[j].Event.Slug })
	return out
}

// Get returns the snapshot for an event, loading it on first use. Loading
// prefers local files and falls back to fetching when they're absent.
func (s *Store) Get(ctx context.Context, slug string) (*Snapshot, error) {
	if snap := s.Peek(slug); snap != nil {
		return snap, nil
	}
	return s.load(ctx, slug, false)
}

// Refresh fetches an event's data from Sessionize and swaps in a rebuilt
// snapshot. On any failure the existing snapshot (if any) is left untouched
// and the error is returned.
func (s *Store) Refresh(ctx context.Context, slug string) (*Snapshot, error) {
	return s.load(ctx, slug, true)
}

func (s *Store) load(ctx context.Context, slug string, force bool) (*Snapshot, error) {
	ev, err := LookupEvent(slug)
	if err != nil {
		return nil, err
	}

	mu := s.builderFor(slug)
	mu.Lock()
	defer mu.Unlock()

	// Someone else may have finished building while we waited for the lock.
	if !force {
		if snap := s.Peek(slug); snap != nil {
			return snap, nil
		}
	}

	snap, err := s.build(ctx, ev, force)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.snaps[slug] = snap
	s.mu.Unlock()
	return snap, nil
}

func (s *Store) builderFor(slug string) *sync.Mutex {
	s.buildMu.Lock()
	defer s.buildMu.Unlock()
	m, ok := s.builders[slug]
	if !ok {
		m = &sync.Mutex{}
		s.builders[slug] = m
	}
	return m
}

// build produces a new snapshot without publishing it.
func (s *Store) build(ctx context.Context, ev Event, force bool) (*Snapshot, error) {
	allPath, spkPath := s.Paths(ev.Slug)
	haveLocal := fileNonEmpty(allPath) && fileNonEmpty(spkPath)

	var (
		data   Data
		source string
		asOf   time.Time
	)

	if force || !haveLocal {
		f := NewFetcher(s.cfg.SessionizeURL, ev.SessionizeCode)
		d, err := f.Fetch(ctx)
		switch {
		case err == nil:
			data, source, asOf = d, "sessionize", time.Now()
			// Cache to disk so a restart doesn't refetch. Best effort: a
			// read-only working directory shouldn't stop us serving.
			if werr := d.Write(allPath, spkPath); werr != nil {
				s.logf("WARNING: %s: could not cache data to disk: %v", ev.Slug, werr)
			}
		case force:
			// An explicit refresh that fails is an error the caller must see;
			// any existing snapshot stays live.
			return nil, fmt.Errorf("refresh %s: %w", ev.Slug, err)
		case !haveLocal:
			return nil, fmt.Errorf("load %s: no local data and fetch failed: %w", ev.Slug, err)
		default:
			s.logf("WARNING: %s: fetch failed (%v); falling back to local files", ev.Slug, err)
		}
	}

	if source == "" {
		allBytes, err := os.ReadFile(allPath)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", allPath, err)
		}
		spkBytes, err := os.ReadFile(spkPath)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", spkPath, err)
		}
		data, source, asOf = Data{All: allBytes, Speakers: spkBytes}, "local", modTime(allPath)
	}

	idx, err := LoadBytes(ev, data.All, data.Speakers)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ev.Slug, err)
	}
	searcher, err := NewSearcher(ctx, idx, s.cfg.Embedder, s.cachePath(ev.Slug))
	if err != nil {
		return nil, fmt.Errorf("%s: build searcher: %w", ev.Slug, err)
	}

	return &Snapshot{
		Event:     ev,
		Index:     idx,
		Searcher:  searcher,
		FetchedAt: asOf,
		Source:    source,
	}, nil
}

// RefreshLoaded refreshes every currently loaded event. Failures are logged and
// skipped — the previous snapshot keeps serving.
func (s *Store) RefreshLoaded(ctx context.Context) {
	for _, prev := range s.Loaded() {
		slug := prev.Event.Slug
		next, err := s.Refresh(ctx, slug)
		if err != nil {
			s.logf("WARNING: auto-refresh %s failed (%v); still serving data from %s",
				slug, err, prev.FetchedAt.Format(time.RFC3339))
			continue
		}
		before, after := len(prev.Index.SessionList), len(next.Index.SessionList)
		if before == after {
			s.logf("auto-refresh %s: %d sessions (unchanged)", slug, after)
		} else {
			s.logf("auto-refresh %s: %d -> %d sessions", slug, before, after)
		}
	}
}

// StartAutoRefresh refreshes every loaded event on an interval until ctx is
// done. A non-positive interval disables it.
func (s *Store) StartAutoRefresh(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	s.logf("auto-refresh: every %s", interval)
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.RefreshLoaded(ctx)
			}
		}
	}()
}

// fileNonEmpty reports whether a path exists and has content — i.e. whether
// falling back to it is viable.
func fileNonEmpty(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Size() > 0
}

// modTime is the file's last-modified time, used as the "as of" stamp for data
// read from disk. Zero if unknown.
func modTime(path string) time.Time {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}
