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

import "strings"

// SessionFilter is the structured filter for find_sessions. Empty fields are
// ignored. All provided fields must match (logical AND).
type SessionFilter struct {
	Company        string `json:"company,omitempty"`        // matches any speaker's company (substring, normalized)
	Speaker        string `json:"speaker,omitempty"`        // matches any speaker's name (fuzzy)
	Day            string `json:"day,omitempty"`            // date prefix, e.g. "2026-11-10"
	Room           string `json:"room,omitempty"`           // substring, case-insensitive
	Track          string `json:"track,omitempty"`          // category/track substring
	IncludeService bool   `json:"includeService,omitempty"` // include registration/breaks etc.
	Limit          int    `json:"limit,omitempty"`          // 0 = no limit
}

// FindSessions applies structured filters and returns matching sessions with
// speakers expanded. Deterministic — this is what answers exact queries like
// "sessions from Google" correctly.
func (idx *Index) FindSessions(f SessionFilter) []SessionResult {
	company := strings.ToLower(strings.TrimSpace(f.Company))
	room := strings.ToLower(strings.TrimSpace(f.Room))
	track := strings.ToLower(strings.TrimSpace(f.Track))
	day := strings.TrimSpace(f.Day)

	var out []SessionResult
	for _, s := range idx.SessionList {
		if s.Service && !f.IncludeService {
			continue
		}
		if day != "" && !strings.HasPrefix(s.StartsAt, day) {
			continue
		}
		if room != "" && !strings.Contains(strings.ToLower(s.Room), room) {
			continue
		}
		if track != "" && !anyContains(s.Tracks, track) {
			continue
		}
		if company != "" && !idx.sessionHasCompany(s, company) {
			continue
		}
		if f.Speaker != "" && !idx.sessionHasSpeaker(s, f.Speaker) {
			continue
		}
		out = append(out, idx.expand(s))
		if f.Limit > 0 && len(out) >= f.Limit {
			break
		}
	}
	return out
}

func (idx *Index) sessionHasCompany(s *Session, companyLower string) bool {
	for _, id := range s.SpeakerIDs {
		if sp := idx.Speakers[id]; sp != nil {
			if strings.Contains(strings.ToLower(sp.Company), companyLower) {
				return true
			}
		}
	}
	return false
}

func (idx *Index) sessionHasSpeaker(s *Session, name string) bool {
	for _, id := range s.SpeakerIDs {
		if sp := idx.Speakers[id]; sp != nil {
			if nameSimilarity(name, sp.Name) >= 0.6 {
				return true
			}
		}
	}
	return false
}

func anyContains(hay []string, needleLower string) bool {
	for _, h := range hay {
		if strings.Contains(strings.ToLower(h), needleLower) {
			return true
		}
	}
	return false
}
