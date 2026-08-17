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
	"testing"
)

// newTestService wires a Service over a fake upstream with two registered
// events, so event routing is actually exercised.
func newTestService(t *testing.T, srv *fakeSessionize) (*Service, Event, Event) {
	t.Helper()

	primary := fakeEvent
	secondary := Event{
		Slug:           "kubecon-china-2027",
		Name:           "KubeCon + CloudNativeCon China 2027",
		Location:       "Hong Kong",
		Dates:          "June 8–10, 2027",
		TZLabel:        "HKT (UTC+8)",
		TZShort:        "HKT",
		SessionizeCode: "cn27code",
		ScheduleURL:    "https://example.org/china/schedule/?id=",
	}
	for _, e := range []Event{primary, secondary} {
		if err := RegisterEvent(e); err != nil {
			t.Fatalf("RegisterEvent(%s): %v", e.Slug, err)
		}
	}
	t.Cleanup(func() {
		eventsMu.Lock()
		delete(events, primary.Slug)
		delete(events, secondary.Slug)
		eventsMu.Unlock()
	})

	store := newTestStore(t, srv.URL, &countingEmbedder{})
	return &Service{Store: store, Default: primary.Slug}, primary, secondary
}

// Omitting the event uses the default; naming one routes to it and loads it on
// demand.
func TestServiceEventRouting(t *testing.T) {
	srv := newFakeSessionize(t, 3)
	svc, primary, secondary := newTestService(t, srv)
	ctx := context.Background()

	def, err := svc.FindSessions(ctx, FindSessionsInput{})
	if err != nil {
		t.Fatalf("FindSessions(default): %v", err)
	}
	if def.Event != primary.Slug {
		t.Errorf("default event = %q, want %q", def.Event, primary.Slug)
	}
	if def.EventName != primary.Name {
		t.Errorf("eventName = %q, want %q", def.EventName, primary.Name)
	}
	if def.DataAsOf == "" {
		t.Error("dataAsOf is empty; agents need it to judge freshness")
	}

	// The second event was never loaded at startup — asking for it should load it.
	if svc.Store.Peek(secondary.Slug) != nil {
		t.Fatal("secondary event should not be loaded yet")
	}
	other, err := svc.FindSessions(ctx, FindSessionsInput{Event: secondary.Slug})
	if err != nil {
		t.Fatalf("FindSessions(%s): %v", secondary.Slug, err)
	}
	if other.Event != secondary.Slug {
		t.Errorf("event = %q, want %q", other.Event, secondary.Slug)
	}
	if svc.Store.Peek(secondary.Slug) == nil {
		t.Error("secondary event was not loaded on demand")
	}

	// Links must come from the event that served the results.
	if len(other.Sessions) == 0 {
		t.Fatal("no sessions returned")
	}
	if got := other.Sessions[0].Link; got[:len(secondary.ScheduleURL)] != secondary.ScheduleURL {
		t.Errorf("session link = %q, want it under %q", got, secondary.ScheduleURL)
	}
}

func TestServiceUnknownEvent(t *testing.T) {
	srv := newFakeSessionize(t, 1)
	svc, _, _ := newTestService(t, srv)

	if _, err := svc.GetSpeaker(context.Background(), GetSpeakerInput{
		Name: "Ada", Event: "kubecon-mars-2099",
	}); err == nil {
		t.Fatal("expected an error for an unknown event")
	}
}

// Every tool response carries freshness metadata.
func TestAllToolsReportFreshness(t *testing.T) {
	srv := newFakeSessionize(t, 4)
	svc, primary, _ := newTestService(t, srv)
	ctx := context.Background()

	find, err := svc.FindSessions(ctx, FindSessionsInput{})
	if err != nil {
		t.Fatalf("FindSessions: %v", err)
	}
	spk, err := svc.GetSpeaker(ctx, GetSpeakerInput{Name: "Ada Lovelace"})
	if err != nil {
		t.Fatalf("GetSpeaker: %v", err)
	}
	search, err := svc.SearchSessions(ctx, SearchSessionsInput{Query: "topic"})
	if err != nil {
		t.Fatalf("SearchSessions: %v", err)
	}

	for name, meta := range map[string]DataMeta{
		"find_sessions":   find.DataMeta,
		"get_speaker":     spk.DataMeta,
		"search_sessions": search.DataMeta,
	} {
		if meta.Event != primary.Slug {
			t.Errorf("%s: event = %q, want %q", name, meta.Event, primary.Slug)
		}
		if meta.DataAsOf == "" {
			t.Errorf("%s: dataAsOf is empty", name)
		}
	}
}

// refresh_data reports the delta so the agent can say what changed.
func TestRefreshDataReportsDelta(t *testing.T) {
	srv := newFakeSessionize(t, 5)
	svc, _, _ := newTestService(t, srv)
	ctx := context.Background()

	if _, err := svc.FindSessions(ctx, FindSessionsInput{}); err != nil {
		t.Fatalf("initial load: %v", err)
	}

	srv.sessions.Store(8)
	out, err := svc.RefreshData(ctx, RefreshDataInput{})
	if err != nil {
		t.Fatalf("RefreshData: %v", err)
	}
	if out.Sessions != 8 {
		t.Errorf("sessions = %d, want 8", out.Sessions)
	}
	if out.SessionsAdded != 3 {
		t.Errorf("sessionsAdded = %d, want 3", out.SessionsAdded)
	}
	if !out.Changed {
		t.Error("changed = false, want true")
	}
	if out.PreviousAsOf == "" {
		t.Error("previousAsOf is empty; the agent can't describe what it replaced")
	}

	// A no-op refresh must report no change rather than pretending it updated.
	noop, err := svc.RefreshData(ctx, RefreshDataInput{})
	if err != nil {
		t.Fatalf("RefreshData (no-op): %v", err)
	}
	if noop.Changed {
		t.Errorf("changed = true after a no-op refresh (added %d sessions)", noop.SessionsAdded)
	}
}

// A failed refresh surfaces the error and keeps serving the old data.
func TestRefreshDataFailureKeepsServing(t *testing.T) {
	srv := newFakeSessionize(t, 6)
	svc, _, _ := newTestService(t, srv)
	ctx := context.Background()

	if _, err := svc.FindSessions(ctx, FindSessionsInput{}); err != nil {
		t.Fatalf("initial load: %v", err)
	}

	srv.fail.Store(true)
	if _, err := svc.RefreshData(ctx, RefreshDataInput{}); err == nil {
		t.Fatal("expected refresh to fail")
	}

	after, err := svc.FindSessions(ctx, FindSessionsInput{})
	if err != nil {
		t.Fatalf("FindSessions after failed refresh: %v", err)
	}
	if after.Count != 6 {
		t.Errorf("count = %d, want the original 6", after.Count)
	}
}

// list_events distinguishes registered-but-unloaded from loaded.
func TestListEventsReportsLoadState(t *testing.T) {
	srv := newFakeSessionize(t, 2)
	svc, primary, secondary := newTestService(t, srv)
	ctx := context.Background()

	if _, err := svc.FindSessions(ctx, FindSessionsInput{}); err != nil {
		t.Fatalf("load primary: %v", err)
	}

	out := svc.ListEvents(ctx)
	if out.Default != primary.Slug {
		t.Errorf("default = %q, want %q", out.Default, primary.Slug)
	}

	byslug := map[string]EventInfo{}
	for _, e := range out.Events {
		byslug[e.Slug] = e
	}

	p, ok := byslug[primary.Slug]
	if !ok {
		t.Fatalf("primary event missing from list_events")
	}
	if !p.Loaded || p.Sessions != 2 || !p.IsDefault {
		t.Errorf("primary = %+v, want loaded with 2 sessions and isDefault", p)
	}

	s, ok := byslug[secondary.Slug]
	if !ok {
		t.Fatalf("secondary event missing from list_events")
	}
	if s.Loaded {
		t.Errorf("secondary reported as loaded, but it was never queried")
	}
	if s.Name != secondary.Name {
		t.Errorf("secondary name = %q, want %q", s.Name, secondary.Name)
	}
}
