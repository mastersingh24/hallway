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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"

	"github.com/gke-demos/hallway/embed"
)

// SearchResult is a session plus the fused relevance score and a short "why".
type SearchResult struct {
	SessionResult
	Score float64 `json:"score"`
}

// Searcher runs hybrid keyword (BM25) + semantic (embedding) search over
// sessions. The embedding layer is optional: with a nil Embedder it degrades to
// keyword-only, so the service always runs.
type Searcher struct {
	idx    *Index
	docs   []*Session // searchable (non-service) sessions, aligned with vecs/bm25
	emb    embed.Embedder
	vecs   [][]float32 // per-doc embeddings (nil if no embedder)
	bm25   *bm25Index
	hasVec bool
}

// NewSearcher builds the BM25 index and, if emb != nil, computes/loads document
// embeddings (cached to cachePath keyed by embedder+content hash).
func NewSearcher(ctx context.Context, idx *Index, emb embed.Embedder, cachePath string) (*Searcher, error) {
	s := &Searcher{idx: idx, emb: emb}
	for _, sess := range idx.SessionList {
		if sess.Service {
			continue
		}
		s.docs = append(s.docs, sess)
	}

	corpus := make([]string, len(s.docs))
	for i, d := range s.docs {
		corpus[i] = d.Title + "\n" + d.Description
	}
	s.bm25 = newBM25(corpus)

	if emb != nil {
		vecs, err := loadOrComputeVectors(ctx, emb, corpus, cachePath)
		if err != nil {
			return nil, err
		}
		s.vecs = vecs
		s.hasVec = len(vecs) == len(s.docs)
	}
	return s, nil
}

// SemanticEnabled reports whether the embedding layer is active.
func (s *Searcher) SemanticEnabled() bool { return s.hasVec }

// Search returns the top-k sessions for a free-text query, fusing keyword and
// semantic rankings with Reciprocal Rank Fusion.
func (s *Searcher) Search(ctx context.Context, query string, k int) ([]SearchResult, error) {
	if k <= 0 {
		k = 10
	}

	// --- keyword ranking (always available) ---
	kwRanked := s.bm25.search(query) // []scored, sorted desc, docIdx + score

	// --- semantic ranking (optional) ---
	var vecRanked []scored
	if s.hasVec {
		qv, err := s.emb.EmbedQuery(ctx, query)
		if err != nil {
			return nil, err
		}
		sims := make([]scored, len(s.docs))
		for i := range s.docs {
			sims[i] = scored{doc: i, score: embed.Cosine(qv, s.vecs[i])}
		}
		sort.Slice(sims, func(a, b int) bool { return sims[a].score > sims[b].score })
		vecRanked = sims
	}

	// --- Reciprocal Rank Fusion ---
	const rrfK = 60.0
	fused := map[int]float64{}
	addRanks := func(list []scored) {
		for rank, sc := range list {
			if sc.score <= 0 {
				continue
			}
			fused[sc.doc] += 1.0 / (rrfK + float64(rank+1))
		}
	}
	addRanks(kwRanked)
	addRanks(vecRanked)

	merged := make([]scored, 0, len(fused))
	for doc, sc := range fused {
		merged = append(merged, scored{doc: doc, score: sc})
	}
	sort.Slice(merged, func(a, b int) bool { return merged[a].score > merged[b].score })

	if len(merged) > k {
		merged = merged[:k]
	}
	out := make([]SearchResult, 0, len(merged))
	for _, m := range merged {
		out = append(out, SearchResult{
			SessionResult: s.idx.expand(s.docs[m.doc]),
			Score:         m.score,
		})
	}
	return out, nil
}

type scored struct {
	doc   int
	score float64
}

// ---- BM25 ----

type bm25Index struct {
	docs    [][]string
	df      map[string]int
	idf     map[string]float64
	avgLen  float64
	docLens []int
	n       int
}

func newBM25(corpus []string) *bm25Index {
	b := &bm25Index{df: map[string]int{}, idf: map[string]float64{}, n: len(corpus)}
	total := 0
	for _, text := range corpus {
		toks := tokens(text)
		b.docs = append(b.docs, toks)
		b.docLens = append(b.docLens, len(toks))
		total += len(toks)
		seen := map[string]bool{}
		for _, t := range toks {
			if !seen[t] {
				b.df[t]++
				seen[t] = true
			}
		}
	}
	if b.n > 0 {
		b.avgLen = float64(total) / float64(b.n)
	}
	for term, df := range b.df {
		b.idf[term] = math.Log(1 + (float64(b.n)-float64(df)+0.5)/(float64(df)+0.5))
	}
	return b
}

func (b *bm25Index) search(query string) []scored {
	const k1, bParam = 1.5, 0.75
	qTerms := tokens(query)
	results := make([]scored, 0, b.n)
	for i, doc := range b.docs {
		tf := map[string]int{}
		for _, t := range doc {
			tf[t]++
		}
		var score float64
		for _, qt := range qTerms {
			f := float64(tf[qt])
			if f == 0 {
				continue
			}
			idf := b.idf[qt]
			denom := f + k1*(1-bParam+bParam*float64(b.docLens[i])/b.avgLen)
			score += idf * (f * (k1 + 1)) / denom
		}
		if score > 0 {
			results = append(results, scored{doc: i, score: score})
		}
	}
	sort.Slice(results, func(a, c int) bool { return results[a].score > results[c].score })
	return results
}

// ---- vector cache ----

// vectorCache stores one vector per *document*, keyed by a hash of that
// document's text. Keying per document (rather than hashing the whole corpus)
// is what makes refresh affordable: when the schedule gains two sessions we
// embed two sessions, not all ~400. The embedder name is stored alongside so
// switching providers or models invalidates the whole file.
type vectorCache struct {
	Embedder string               `json:"embedder"`
	Vectors  map[string][]float32 `json:"vectors"` // docHash -> embedding
}

// docHash identifies a document by its exact text.
func docHash(text string) string {
	h := sha256.Sum256([]byte(text))
	return hex.EncodeToString(h[:])
}

// loadOrComputeVectors returns one vector per corpus entry, embedding only the
// documents missing from the cache and writing the merged result back.
func loadOrComputeVectors(ctx context.Context, emb embed.Embedder, corpus []string, cachePath string) ([][]float32, error) {
	cached := map[string][]float32{}
	if cachePath != "" {
		if raw, err := os.ReadFile(cachePath); err == nil {
			var vc vectorCache
			// A different embedder means the cached vectors live in another
			// space entirely — drop them rather than mixing.
			if json.Unmarshal(raw, &vc) == nil && vc.Embedder == emb.Name() && vc.Vectors != nil {
				cached = vc.Vectors
			}
		}
	}

	hashes := make([]string, len(corpus))
	for i, text := range corpus {
		hashes[i] = docHash(text)
	}

	// Collect the misses, deduplicated: two sessions with identical text embed once.
	var missTexts []string
	var missHashes []string
	pending := map[string]bool{}
	for i, h := range hashes {
		if _, ok := cached[h]; ok || pending[h] {
			continue
		}
		pending[h] = true
		missHashes = append(missHashes, h)
		missTexts = append(missTexts, corpus[i])
	}

	if len(missTexts) > 0 {
		fresh, err := emb.Embed(ctx, missTexts)
		if err != nil {
			return nil, err
		}
		if len(fresh) != len(missTexts) {
			return nil, fmt.Errorf("embedder returned %d vectors for %d documents", len(fresh), len(missTexts))
		}
		for i, h := range missHashes {
			cached[h] = fresh[i]
		}
	}

	out := make([][]float32, len(corpus))
	for i, h := range hashes {
		out[i] = cached[h]
	}

	// Persist pruned to the current corpus so the file can't grow without bound
	// across refreshes. Only rewrite when something actually changed.
	if cachePath != "" && len(missTexts) > 0 {
		keep := make(map[string][]float32, len(hashes))
		for _, h := range hashes {
			keep[h] = cached[h]
		}
		if raw, err := json.Marshal(vectorCache{Embedder: emb.Name(), Vectors: keep}); err == nil {
			_ = writeFileAtomic(cachePath, raw)
		}
	}
	return out, nil
}
