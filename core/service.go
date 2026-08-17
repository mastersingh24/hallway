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
	"math"
	"time"
)

// Service is the façade the transports (REST + MCP) call. It owns the snapshot
// store and exposes the tools with typed input/output. Keeping the I/O types
// here means REST and MCP expose identical contracts.
//
// Every tool takes an optional event slug. Omitted, it uses Default — so a
// single-conference deployment behaves exactly as before, while an agent that
// knows about other events can ask for them and have them loaded on demand.
type Service struct {
	Store   *Store
	Default string
}

// resolve loads the snapshot for a (possibly empty) event slug.
func (s *Service) resolve(ctx context.Context, slug string) (*Snapshot, error) {
	if slug == "" {
		slug = s.Default
	}
	return s.Store.Get(ctx, slug)
}

// DataMeta rides along on every tool response so the agent can tell how fresh
// the answer is — and hedge, or call refresh_data, when it isn't.
type DataMeta struct {
	Event     string  `json:"event" jsonschema:"slug of the event these results came from"`
	EventName string  `json:"eventName" jsonschema:"full display name of the event"`
	DataAsOf  string  `json:"dataAsOf" jsonschema:"RFC3339 timestamp of when this data was obtained"`
	DataAgeHr float64 `json:"dataAgeHours" jsonschema:"how many hours old the data is"`
}

func metaOf(snap *Snapshot) DataMeta {
	return DataMeta{
		Event:     snap.Event.Slug,
		EventName: snap.Event.Name,
		DataAsOf:  snap.FetchedAt.UTC().Format(time.RFC3339),
		DataAgeHr: math.Round(snap.Age().Hours()*10) / 10,
	}
}

// ---- find_sessions ----

// FindSessionsInput is a structured filter plus an optional event override.
type FindSessionsInput struct {
	SessionFilter
	Event string `json:"event,omitempty" jsonschema:"event slug to query; omit for the default event. Call list_events to see the options"`
}

// FindSessionsOutput wraps structured-filter results.
type FindSessionsOutput struct {
	DataMeta
	Count    int             `json:"count"`
	Sessions []SessionResult `json:"sessions"`
}

// FindSessions applies exact structured filters (company, speaker, day, room, track).
func (s *Service) FindSessions(ctx context.Context, in FindSessionsInput) (FindSessionsOutput, error) {
	snap, err := s.resolve(ctx, in.Event)
	if err != nil {
		return FindSessionsOutput{}, err
	}
	sessions := snap.Index.FindSessions(in.SessionFilter)
	return FindSessionsOutput{
		DataMeta: metaOf(snap),
		Count:    len(sessions),
		Sessions: sessions,
	}, nil
}

// ---- get_speaker ----

// GetSpeakerInput resolves a (possibly fuzzy) speaker name.
type GetSpeakerInput struct {
	Name      string  `json:"name" jsonschema:"the speaker's name; fuzzy/misspelled names are tolerated"`
	Threshold float64 `json:"threshold,omitempty" jsonschema:"minimum match score 0..1 (default 0.5)"`
	Event     string  `json:"event,omitempty" jsonschema:"event slug to query; omit for the default event"`
}

// GetSpeakerOutput wraps ranked speaker matches with their sessions.
type GetSpeakerOutput struct {
	DataMeta
	Count   int            `json:"count"`
	Matches []SpeakerMatch `json:"matches"`
}

// GetSpeaker resolves a name to speakers and their sessions, ranked by confidence.
func (s *Service) GetSpeaker(ctx context.Context, in GetSpeakerInput) (GetSpeakerOutput, error) {
	snap, err := s.resolve(ctx, in.Event)
	if err != nil {
		return GetSpeakerOutput{}, err
	}
	m := snap.Index.GetSpeaker(in.Name, in.Threshold)
	return GetSpeakerOutput{
		DataMeta: metaOf(snap),
		Count:    len(m),
		Matches:  m,
	}, nil
}

// ---- search_sessions ----

// SearchSessionsInput is a free-text topic query.
type SearchSessionsInput struct {
	Query string `json:"query" jsonschema:"free-text topic, e.g. 'service mesh security' or 'eBPF observability'"`
	K     int    `json:"k,omitempty" jsonschema:"number of results to return (default 10)"`
	Event string `json:"event,omitempty" jsonschema:"event slug to query; omit for the default event"`
}

// SearchSessionsOutput wraps ranked hybrid-search results.
type SearchSessionsOutput struct {
	DataMeta
	Count           int            `json:"count"`
	SemanticEnabled bool           `json:"semanticEnabled"`
	Results         []SearchResult `json:"results"`
}

// SearchSessions runs hybrid keyword+semantic search over session titles/abstracts.
func (s *Service) SearchSessions(ctx context.Context, in SearchSessionsInput) (SearchSessionsOutput, error) {
	snap, err := s.resolve(ctx, in.Event)
	if err != nil {
		return SearchSessionsOutput{}, err
	}
	res, err := snap.Searcher.Search(ctx, in.Query, in.K)
	if err != nil {
		return SearchSessionsOutput{}, err
	}
	return SearchSessionsOutput{
		DataMeta:        metaOf(snap),
		Count:           len(res),
		SemanticEnabled: snap.Searcher.SemanticEnabled(),
		Results:         res,
	}, nil
}

// ---- list_events ----

// EventInfo describes one registered conference and, if loaded, its data state.
type EventInfo struct {
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	Location  string `json:"location,omitempty"`
	Dates     string `json:"dates,omitempty"`
	Timezone  string `json:"timezone,omitempty"`
	IsDefault bool   `json:"isDefault"`
	Loaded    bool   `json:"loaded" jsonschema:"whether this event's data is in memory; unloaded events load on first query"`

	// Populated only when Loaded.
	DataAsOf        string  `json:"dataAsOf,omitempty"`
	DataAgeHr       float64 `json:"dataAgeHours,omitempty"`
	Source          string  `json:"source,omitempty" jsonschema:"where the loaded data came from: sessionize or local"`
	Sessions        int     `json:"sessions,omitempty"`
	Speakers        int     `json:"speakers,omitempty"`
	SemanticEnabled bool    `json:"semanticEnabled,omitempty"`
}

// ListEventsInput takes no arguments.
type ListEventsInput struct{}

// ListEventsOutput lists every registered conference.
type ListEventsOutput struct {
	Default string      `json:"default"`
	Count   int         `json:"count"`
	Events  []EventInfo `json:"events"`
}

// ListEvents reports which conferences this server can answer questions about
// and how fresh each loaded one is.
func (s *Service) ListEvents(context.Context) ListEventsOutput {
	all := AllEvents()
	out := ListEventsOutput{Default: s.Default, Count: len(all)}
	for _, ev := range all {
		info := EventInfo{
			Slug:      ev.Slug,
			Name:      ev.Name,
			Location:  ev.Location,
			Dates:     ev.Dates,
			Timezone:  ev.TZLabel,
			IsDefault: ev.Slug == s.Default,
		}
		if snap := s.Store.Peek(ev.Slug); snap != nil {
			info.Loaded = true
			info.DataAsOf = snap.FetchedAt.UTC().Format(time.RFC3339)
			info.DataAgeHr = math.Round(snap.Age().Hours()*10) / 10
			info.Source = snap.Source
			info.Sessions = len(snap.Index.SessionList)
			info.Speakers = len(snap.Index.SpeakerList)
			info.SemanticEnabled = snap.Searcher.SemanticEnabled()
		}
		out.Events = append(out.Events, info)
	}
	return out
}

// ---- refresh_data ----

// RefreshDataInput selects which event to re-fetch.
type RefreshDataInput struct {
	Event string `json:"event,omitempty" jsonschema:"event slug to refresh; omit for the default event"`
}

// RefreshDataOutput reports what the refresh changed.
type RefreshDataOutput struct {
	DataMeta
	Sessions      int    `json:"sessions"`
	Speakers      int    `json:"speakers"`
	PreviousAsOf  string `json:"previousAsOf,omitempty" jsonschema:"when the replaced data was obtained, if any"`
	SessionsAdded int    `json:"sessionsAdded" jsonschema:"net change in session count (may be negative)"`
	SpeakersAdded int    `json:"speakersAdded" jsonschema:"net change in speaker count (may be negative)"`
	Changed       bool   `json:"changed" jsonschema:"whether the counts differ from the previous snapshot"`
}

// RefreshData re-fetches an event from Sessionize and swaps in the rebuilt
// snapshot. On failure the previous data keeps serving and an error is returned.
func (s *Service) RefreshData(ctx context.Context, in RefreshDataInput) (RefreshDataOutput, error) {
	slug := in.Event
	if slug == "" {
		slug = s.Default
	}

	prev := s.Store.Peek(slug)
	snap, err := s.Store.Refresh(ctx, slug)
	if err != nil {
		return RefreshDataOutput{}, err
	}

	out := RefreshDataOutput{
		DataMeta: metaOf(snap),
		Sessions: len(snap.Index.SessionList),
		Speakers: len(snap.Index.SpeakerList),
	}
	if prev != nil {
		out.PreviousAsOf = prev.FetchedAt.UTC().Format(time.RFC3339)
		out.SessionsAdded = out.Sessions - len(prev.Index.SessionList)
		out.SpeakersAdded = out.Speakers - len(prev.Index.SpeakerList)
		out.Changed = out.SessionsAdded != 0 || out.SpeakersAdded != 0
	} else {
		out.Changed = true
	}
	return out, nil
}
