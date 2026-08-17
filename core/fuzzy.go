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
	"strings"
	"unicode"
)

// normalizeName lowercases, strips accents/punctuation, and collapses spaces so
// "Tim  Hockin", "tim hockin", and "Tim Hockin." all match.
func normalizeName(s string) string {
	var b strings.Builder
	prevSpace := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			prevSpace = false
		case unicode.IsSpace(r):
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
		default:
			// drop punctuation
		}
	}
	return strings.TrimSpace(b.String())
}

// tokens splits normalized text into a set of words.
func tokens(s string) []string {
	return strings.Fields(normalizeName(s))
}

// levenshtein returns the edit distance between a and b.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	la, lb := len(ra), len(rb)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			curr[j] = min3(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[lb]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

// nameSimilarity returns a 0..1 score combining token-set overlap and edit
// distance, tuned for person-name matching ("Tim Hockin" vs "Timothy Hockin").
func nameSimilarity(query, candidate string) float64 {
	q := normalizeName(query)
	c := normalizeName(candidate)
	if q == "" || c == "" {
		return 0
	}
	if q == c {
		return 1
	}
	if strings.Contains(c, q) || strings.Contains(q, c) {
		return 0.9
	}

	// token-set overlap: fraction of query tokens that (fuzzily) hit a candidate token
	qt, ct := tokens(q), tokens(c)
	if len(qt) == 0 {
		return 0
	}
	hits := 0
	for _, a := range qt {
		for _, b := range ct {
			if a == b || strings.HasPrefix(b, a) || strings.HasPrefix(a, b) {
				hits++
				break
			}
			// allow small typos on longer tokens
			if len(a) >= 4 && len(b) >= 4 && levenshtein(a, b) <= 1 {
				hits++
				break
			}
		}
	}
	tokenScore := float64(hits) / float64(len(qt))

	// whole-string edit ratio as a tiebreaker
	dist := levenshtein(q, c)
	maxLen := len(q)
	if len(c) > maxLen {
		maxLen = len(c)
	}
	editScore := 1 - float64(dist)/float64(maxLen)
	if editScore < 0 {
		editScore = 0
	}

	return 0.7*tokenScore + 0.3*editScore
}
