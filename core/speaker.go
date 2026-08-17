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

import "sort"

// SpeakerMatch is a resolved speaker plus how confident the name match was.
// The embedded Speaker contributes id/name/company/tagLine/sessionIds; Sessions
// holds those sessions fully expanded. (Field is named SessionDetails to avoid a
// Go field-name collision with the embedded Speaker.Sessions.)
type SpeakerMatch struct {
	Speaker
	Score          float64         `json:"score"`
	SessionDetails []SessionResult `json:"sessions"`
}

// GetSpeaker resolves a (possibly fuzzy/misspelled) name to speakers and their
// sessions. It returns candidates ranked by confidence — the caller/agent can
// take the top hit or disambiguate. threshold defaults to 0.5 when <= 0.
func (idx *Index) GetSpeaker(name string, threshold float64) []SpeakerMatch {
	if threshold <= 0 {
		threshold = 0.5
	}

	// exact normalized hit first
	if sp := idx.nameIndex[normalizeName(name)]; sp != nil {
		return []SpeakerMatch{idx.speakerMatch(sp, 1)}
	}

	var out []SpeakerMatch
	for _, sp := range idx.SpeakerList {
		if score := nameSimilarity(name, sp.Name); score >= threshold {
			out = append(out, idx.speakerMatch(sp, score))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > 10 {
		out = out[:10]
	}
	return out
}

func (idx *Index) speakerMatch(sp *Speaker, score float64) SpeakerMatch {
	m := SpeakerMatch{Speaker: *sp, Score: score}
	for _, sid := range sp.Sessions {
		if s := idx.Sessions[sid]; s != nil {
			m.SessionDetails = append(m.SessionDetails, idx.expand(s))
		}
	}
	return m
}
