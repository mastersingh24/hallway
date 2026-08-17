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

// Package embed provides pluggable text embeddings for the semantic layer of
// hybrid search. The Embedder interface is provider-agnostic; Gemini is the
// default implementation. When no provider/key is configured, the search layer
// degrades gracefully to keyword-only (BM25) ranking.
package embed

import (
	"context"
	"math"
)

// Embedder turns texts into dense vectors. Implementations must return one
// vector per input, in order.
type Embedder interface {
	// Embed embeds documents (for indexing).
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	// EmbedQuery embeds a single search query (some providers use a distinct
	// task type for queries vs. documents).
	EmbedQuery(ctx context.Context, text string) ([]float32, error)
	// Name identifies the provider/model, used as part of the cache key.
	Name() string
}

// Cosine returns cosine similarity between two equal-length vectors.
func Cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
