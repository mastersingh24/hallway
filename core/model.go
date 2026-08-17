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

// Package core holds the LLM-agnostic query engine over the KubeCon /
// Sessionize schedule data: loading + joining the raw JSON, structured
// filters, fuzzy speaker resolution, and hybrid (keyword + embedding) search.
//
// Nothing in this package knows about HTTP, MCP, or any particular LLM. The
// transport layer (REST + MCP) and any agent client sit on top of it.
package core

// Speaker is a resolved, query-friendly view of a Sessionize speaker.
type Speaker struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Company  string   `json:"company"`
	TagLine  string   `json:"tagLine,omitempty"`
	Sessions []string `json:"sessionIds"`
}

// Session is a resolved, query-friendly view of a Sessionize session with
// rooms and category items already dereferenced to human-readable names.
type Session struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	StartsAt    string   `json:"startsAt"` // RFC3339-ish local time, e.g. 2026-11-10T14:00:00
	EndsAt      string   `json:"endsAt"`
	Room        string   `json:"room,omitempty"`
	Tracks      []string `json:"tracks,omitempty"`
	SpeakerIDs  []string `json:"speakerIds"`
	Status      string   `json:"status,omitempty"`
	Service     bool     `json:"isServiceSession"`

	// Link is the public deep-link to the session on the LF schedule page.
	Link string `json:"link"`
}

// SpeakerRef is a speaker embedded inside a session result.
type SpeakerRef struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Company string `json:"company"`
}

// SessionResult is a Session with its speakers expanded, as returned by tools.
type SessionResult struct {
	Session
	Speakers []SpeakerRef `json:"speakers"`
}
