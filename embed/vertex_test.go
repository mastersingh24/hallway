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

package embed

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Vertex rejects a request whose instances exceed the model's total token
// budget, so batches must be bounded by estimated tokens and not just by count.
func TestChunkEndRespectsTokenBudget(t *testing.T) {
	// ~1000 chars ≈ 250 tokens each: the token budget bites long before the
	// 100-instance cap does.
	texts := make([]string, 300)
	for i := range texts {
		texts[i] = strings.Repeat("a", 1000)
	}

	for start := 0; start < len(texts); {
		end := chunkEnd(texts, start)
		if end <= start {
			t.Fatalf("chunkEnd(%d) = %d, must advance", start, end)
		}
		if n := end - start; n > maxInstances {
			t.Errorf("chunk [%d,%d) has %d instances, over the %d cap", start, end, n, maxInstances)
		}
		tokens := 0
		for _, s := range texts[start:end] {
			tokens += estimateTokens(s)
		}
		if end-start > 1 && tokens > maxChunkTokens {
			t.Errorf("chunk [%d,%d) is ~%d tokens, over the %d budget", start, end, tokens, maxChunkTokens)
		}
		start = end
	}
}

// A single document larger than the whole budget must still be emitted, alone,
// rather than stalling the loop.
func TestChunkEndAlwaysAdvances(t *testing.T) {
	huge := strings.Repeat("b", maxChunkTokens*charsPerToken*3)
	texts := []string{huge, huge}
	if got := chunkEnd(texts, 0); got != 1 {
		t.Errorf("chunkEnd = %d, want 1 (oversized doc batched alone)", got)
	}
}

func TestClampInstance(t *testing.T) {
	short := "a short session abstract"
	if got := clampInstance(short); got != short {
		t.Errorf("clampInstance shortened a short input")
	}

	// Multi-byte runes must not be split, or the API sees invalid UTF-8.
	long := strings.Repeat("é", maxInstanceChars)
	got := clampInstance(long)
	if len(got) > maxInstanceChars {
		t.Errorf("clamped length = %d, want <= %d", len(got), maxInstanceChars)
	}
	if !utf8.ValidString(got) {
		t.Error("clampInstance produced invalid UTF-8")
	}
}
