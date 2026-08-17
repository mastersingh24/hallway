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
	"sync"
)

// Event describes a single conference: everything that differs between
// KubeCon NA, EU, China, or any other Sessionize-hosted event. Adding support
// for a new event is one entry in the registry below plus its Sessionize code.
type Event struct {
	// Slug is the short identifier used with -event, e.g. "kubecon-na-2026".
	Slug string

	// Name is the full display title, e.g.
	// "KubeCon + CloudNativeCon North America 2026".
	Name string

	// Location is the human-readable venue/city, e.g. "Salt Lake City, Utah".
	Location string

	// Dates is the human-readable date range, e.g. "November 9–12, 2026".
	Dates string

	// TZLabel is the timezone the published times are in, e.g. "MST (UTC-7)".
	// Sessionize emits local times with no offset, so this is a label only.
	TZLabel string

	// TZShort is the short form used in table cells, e.g. "MST".
	TZShort string

	// SessionizeCode is the event's Sessionize API code, e.g. "svi82w6c".
	SessionizeCode string

	// ScheduleURL is the public schedule page; a session id is appended to it
	// to build each session's deep link.
	ScheduleURL string
}

// SessionLink returns the public deep-link for a session id.
func (e Event) SessionLink(sessionID string) string {
	return e.ScheduleURL + sessionID
}

// events is the built-in registry. Add a new conference here.
var events = map[string]Event{
	"kubecon-na-2026": {
		Slug:           "kubecon-na-2026",
		Name:           "KubeCon + CloudNativeCon North America 2026",
		Location:       "Salt Lake City, Utah",
		Dates:          "November 9–12, 2026",
		TZLabel:        "MST (UTC-7)",
		TZShort:        "MST",
		SessionizeCode: "svi82w6c",
		ScheduleURL:    "https://events.linuxfoundation.org/kubecon-cloudnativecon-north-america/program/schedule/?id=",
	},
}

// eventsMu guards the registry. Tools can now name an event per call, so
// lookups happen on request goroutines, not just at startup.
var eventsMu sync.RWMutex

// DefaultEventSlug is used when -event is not supplied.
const DefaultEventSlug = "kubecon-na-2026"

// LookupEvent returns the registered event for a slug.
func LookupEvent(slug string) (Event, error) {
	eventsMu.RLock()
	e, ok := events[slug]
	eventsMu.RUnlock()
	if !ok {
		return Event{}, fmt.Errorf("unknown event %q; known events: %s", slug, strings.Join(EventSlugs(), ", "))
	}
	return e, nil
}

// EventSlugs lists the registered event slugs, sorted.
func EventSlugs() []string {
	eventsMu.RLock()
	defer eventsMu.RUnlock()
	out := make([]string, 0, len(events))
	for k := range events {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// AllEvents returns every registered event, ordered by slug.
func AllEvents() []Event {
	eventsMu.RLock()
	defer eventsMu.RUnlock()
	out := make([]Event, 0, len(events))
	for _, e := range events {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out
}

// RegisterEvent adds or replaces an event in the registry. Registering a new
// conference needs no code change: put it in an -events JSON file.
func RegisterEvent(e Event) error {
	switch {
	case strings.TrimSpace(e.Slug) == "":
		return fmt.Errorf("event: slug is required")
	case strings.TrimSpace(e.Name) == "":
		return fmt.Errorf("event %q: name is required", e.Slug)
	case strings.TrimSpace(e.SessionizeCode) == "":
		return fmt.Errorf("event %q: sessionizeCode is required", e.Slug)
	}
	if e.TZShort == "" {
		e.TZShort = e.TZLabel
	}
	eventsMu.Lock()
	defer eventsMu.Unlock()
	events[e.Slug] = e
	return nil
}

// eventFile is the on-disk shape of an -events file: a JSON array of events,
// using lowerCamelCase keys.
type eventFile struct {
	Slug           string `json:"slug"`
	Name           string `json:"name"`
	Location       string `json:"location"`
	Dates          string `json:"dates"`
	TZLabel        string `json:"tzLabel"`
	TZShort        string `json:"tzShort"`
	SessionizeCode string `json:"sessionizeCode"`
	ScheduleURL    string `json:"scheduleUrl"`
}

// LoadEventsFile registers every event described in a JSON file, returning how
// many were added. Entries override built-ins with the same slug.
func LoadEventsFile(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read events file: %w", err)
	}
	var list []eventFile
	if err := json.Unmarshal(raw, &list); err != nil {
		return 0, fmt.Errorf("parse %s: %w (expected a JSON array of events)", path, err)
	}
	for _, f := range list {
		if err := RegisterEvent(Event{
			Slug:           f.Slug,
			Name:           f.Name,
			Location:       f.Location,
			Dates:          f.Dates,
			TZLabel:        f.TZLabel,
			TZShort:        f.TZShort,
			SessionizeCode: f.SessionizeCode,
			ScheduleURL:    f.ScheduleURL,
		}); err != nil {
			return 0, fmt.Errorf("%s: %w", path, err)
		}
	}
	return len(list), nil
}
