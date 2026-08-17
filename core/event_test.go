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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeEvent is a deliberately non-NA event: if anything is still hardcoded to
// KubeCon North America 2026, these tests fail.
var fakeEvent = Event{
	Slug:           "kubecon-eu-2027",
	Name:           "KubeCon + CloudNativeCon Europe 2027",
	Location:       "Amsterdam, Netherlands",
	Dates:          "March 23–26, 2027",
	TZLabel:        "CET (UTC+1)",
	TZShort:        "CET",
	SessionizeCode: "eu27code",
	ScheduleURL:    "https://events.linuxfoundation.org/kubecon-cloudnativecon-europe/program/schedule/?id=",
}

func TestSessionLinkUsesEvent(t *testing.T) {
	got := fakeEvent.SessionLink("12345")
	want := "https://events.linuxfoundation.org/kubecon-cloudnativecon-europe/program/schedule/?id=12345"
	if got != want {
		t.Errorf("SessionLink() = %q, want %q", got, want)
	}
	if strings.Contains(got, "north-america") {
		t.Errorf("SessionLink() leaked the North America URL: %q", got)
	}
}

// writeFixture creates a minimal but realistic pair of Sessionize exports.
func writeFixture(t *testing.T) (allPath, spkPath string) {
	t.Helper()
	dir := t.TempDir()

	all := map[string]any{
		"sessions": []map[string]any{{
			"id": "999", "title": "Keynote: Something Cloud Native",
			"description": "A talk.",
			"startsAt":    "2027-03-23T09:00:00", "endsAt": "2027-03-23T09:30:00",
			"speakers": []string{"spk-1"}, "categoryItems": []int{1}, "roomId": 7,
			"status": "Accepted",
		}},
		"rooms":      []map[string]any{{"id": 7, "name": "Hall A"}},
		"categories": []map[string]any{{"title": "Track", "items": []map[string]any{{"id": 1, "name": "Platform"}}}},
	}
	speakers := []map[string]any{{
		"id": "spk-1", "fullName": "Ada Lovelace", "tagLine": "Engineer",
		"questionAnswers": []map[string]any{{"question": "Company", "answer": "Google"}},
	}}

	allPath = filepath.Join(dir, "all.json")
	spkPath = filepath.Join(dir, "speakers.json")
	for path, v := range map[string]any{allPath: all, spkPath: speakers} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return allPath, spkPath
}

func TestLoadAppliesEventToLinks(t *testing.T) {
	allPath, spkPath := writeFixture(t)

	idx, err := Load(fakeEvent, allPath, spkPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if idx.Event.Slug != fakeEvent.Slug {
		t.Errorf("Index.Event.Slug = %q, want %q", idx.Event.Slug, fakeEvent.Slug)
	}

	s := idx.Sessions["999"]
	if s == nil {
		t.Fatal("session 999 not loaded")
	}
	if !strings.Contains(s.Link, "kubecon-cloudnativecon-europe") {
		t.Errorf("session link not built from the event: %q", s.Link)
	}
	if strings.Contains(s.Link, "north-america") {
		t.Errorf("session link still hardcoded to North America: %q", s.Link)
	}

	// The speaker<->session join and company resolution should still work.
	if got := idx.Speakers["spk-1"].Company; got != "Google" {
		t.Errorf("company = %q, want %q", got, "Google")
	}
	if got := len(idx.FindSessions(SessionFilter{Company: "Google"})); got != 1 {
		t.Errorf("FindSessions(company=Google) returned %d sessions, want 1", got)
	}
}

func TestLookupEventUnknown(t *testing.T) {
	if _, err := LookupEvent("nope-2099"); err == nil {
		t.Fatal("expected an error for an unknown event slug")
	} else if !strings.Contains(err.Error(), "known events") {
		t.Errorf("error should list known events, got: %v", err)
	}
}

func TestDefaultEventIsRegistered(t *testing.T) {
	if _, err := LookupEvent(DefaultEventSlug); err != nil {
		t.Fatalf("DefaultEventSlug %q is not registered: %v", DefaultEventSlug, err)
	}
}
