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
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// ---- raw Sessionize shapes (only the fields we use) ----

type rawAll struct {
	Sessions   []rawSession  `json:"sessions"`
	Rooms      []rawRoom     `json:"rooms"`
	Categories []rawCategory `json:"categories"`
}

type rawSession struct {
	ID               string   `json:"id"`
	Title            string   `json:"title"`
	Description      string   `json:"description"`
	StartsAt         string   `json:"startsAt"`
	EndsAt           string   `json:"endsAt"`
	IsServiceSession bool     `json:"isServiceSession"`
	Speakers         []string `json:"speakers"`
	CategoryItems    []int    `json:"categoryItems"`
	RoomID           int      `json:"roomId"`
	Status           string   `json:"status"`
}

type rawRoom struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type rawCategory struct {
	Title string `json:"title"`
	Items []struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	} `json:"items"`
}

type rawSpeaker struct {
	ID              string `json:"id"`
	FullName        string `json:"fullName"`
	TagLine         string `json:"tagLine"`
	QuestionAnswers []struct {
		Question string `json:"question"`
		Answer   string `json:"answer"`
	} `json:"questionAnswers"`
}

// Index is the immutable, in-memory query index. Safe for concurrent reads.
type Index struct {
	// Event is the conference this index describes.
	Event Event

	Sessions    map[string]*Session
	Speakers    map[string]*Speaker
	SessionList []*Session // sorted by StartsAt
	SpeakerList []*Speaker // sorted by Name

	nameIndex map[string]*Speaker // normalized full name -> speaker (exact)
}

func company(qa []struct {
	Question string `json:"question"`
	Answer   string `json:"answer"`
}) string {
	m := map[string]string{}
	for _, q := range qa {
		m[q.Question] = strings.TrimSpace(q.Answer)
	}
	if v := m["Company Override"]; v != "" {
		return v
	}
	return m["Company"]
}

// Load reads all.json and speakers.json and builds the joined index for the
// given event. The event supplies the deep-link base used for session links.
func Load(ev Event, allPath, speakersPath string) (*Index, error) {
	allBytes, err := os.ReadFile(allPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", allPath, err)
	}
	spkBytes, err := os.ReadFile(speakersPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", speakersPath, err)
	}
	return LoadBytes(ev, allBytes, spkBytes)
}

// LoadBytes builds the joined index straight from the two Sessionize payloads,
// without touching the filesystem. Load is a thin wrapper over it; a refreshing
// server uses it to rebuild from a freshly fetched Data.
func LoadBytes(ev Event, allBytes, spkBytes []byte) (*Index, error) {
	var all rawAll
	if err := json.Unmarshal(allBytes, &all); err != nil {
		return nil, fmt.Errorf("parse all.json: %w", err)
	}
	var rawSpeakers []rawSpeaker
	if err := json.Unmarshal(spkBytes, &rawSpeakers); err != nil {
		return nil, fmt.Errorf("parse speakers.json: %w", err)
	}

	roomByID := map[int]string{}
	for _, r := range all.Rooms {
		roomByID[r.ID] = r.Name
	}
	catByID := map[int]string{}
	for _, c := range all.Categories {
		for _, it := range c.Items {
			catByID[it.ID] = it.Name
		}
	}

	idx := &Index{
		Event:     ev,
		Sessions:  map[string]*Session{},
		Speakers:  map[string]*Speaker{},
		nameIndex: map[string]*Speaker{},
	}

	for _, sp := range rawSpeakers {
		s := &Speaker{
			ID:      sp.ID,
			Name:    strings.TrimSpace(sp.FullName),
			Company: company(sp.QuestionAnswers),
			TagLine: strings.TrimSpace(sp.TagLine),
		}
		idx.Speakers[s.ID] = s
		idx.SpeakerList = append(idx.SpeakerList, s)
		idx.nameIndex[normalizeName(s.Name)] = s
	}

	for _, rs := range all.Sessions {
		var tracks []string
		for _, ci := range rs.CategoryItems {
			if name := catByID[ci]; name != "" {
				tracks = append(tracks, name)
			}
		}
		s := &Session{
			ID:          rs.ID,
			Title:       strings.TrimSpace(rs.Title),
			Description: strings.TrimSpace(rs.Description),
			StartsAt:    rs.StartsAt,
			EndsAt:      rs.EndsAt,
			Room:        roomByID[rs.RoomID],
			Tracks:      tracks,
			SpeakerIDs:  rs.Speakers,
			Status:      rs.Status,
			Service:     rs.IsServiceSession,
			Link:        ev.SessionLink(rs.ID),
		}
		idx.Sessions[s.ID] = s
		idx.SessionList = append(idx.SessionList, s)

		// authoritative speaker -> session join
		for _, spid := range rs.Speakers {
			if sp := idx.Speakers[spid]; sp != nil {
				sp.Sessions = append(sp.Sessions, s.ID)
			}
		}
	}

	// Stable sorts: sessions sharing a start time keep their published order,
	// so regenerated reports don't churn between runs.
	sort.SliceStable(idx.SessionList, func(i, j int) bool {
		return idx.SessionList[i].StartsAt < idx.SessionList[j].StartsAt
	})
	sort.SliceStable(idx.SpeakerList, func(i, j int) bool {
		return idx.SpeakerList[i].Name < idx.SpeakerList[j].Name
	})

	return idx, nil
}

// expand turns a Session into a SessionResult with speakers dereferenced.
func (idx *Index) expand(s *Session) SessionResult {
	res := SessionResult{Session: *s}
	for _, id := range s.SpeakerIDs {
		if sp := idx.Speakers[id]; sp != nil {
			res.Speakers = append(res.Speakers, SpeakerRef{ID: sp.ID, Name: sp.Name, Company: sp.Company})
		}
	}
	return res
}
