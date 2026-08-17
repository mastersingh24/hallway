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

// Package transport exposes the core Service over REST (for web chatbots / any
// agent framework) and MCP (for MCP-capable clients like Claude Desktop). Both
// front doors call the same core.Service, so the tool contracts are identical.
package transport

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gke-demos/hallway/core"
)

// RESTHandler returns an http.Handler exposing the tools plus a health check
// and a machine-readable tool catalog (useful for agent frameworks that want to
// discover tool schemas).
func RESTHandler(svc *core.Service) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		// Report on what's loaded rather than forcing a load — /health must stay
		// cheap and must not fail because an event's data is unreachable.
		events := []map[string]any{}
		for _, snap := range svc.Store.Loaded() {
			events = append(events, map[string]any{
				"event":           snap.Event.Slug,
				"sessions":        len(snap.Index.SessionList),
				"speakers":        len(snap.Index.SpeakerList),
				"semanticEnabled": snap.Searcher.SemanticEnabled(),
				"dataAsOf":        snap.FetchedAt.UTC(),
				"source":          snap.Source,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "ok",
			"default": svc.Default,
			"known":   core.EventSlugs(),
			"loaded":  events,
		})
	})

	mux.HandleFunc("GET /tools", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, toolCatalog())
	})

	mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, svc.ListEvents(r.Context()))
	})

	mux.HandleFunc("POST /tools/find_sessions", func(w http.ResponseWriter, r *http.Request) {
		var in core.FindSessionsInput
		if !decode(w, r, &in) {
			return
		}
		out, err := svc.FindSessions(r.Context(), in)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	})

	mux.HandleFunc("POST /tools/get_speaker", func(w http.ResponseWriter, r *http.Request) {
		var in core.GetSpeakerInput
		if !decode(w, r, &in) {
			return
		}
		out, err := svc.GetSpeaker(r.Context(), in)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	})

	mux.HandleFunc("POST /tools/search_sessions", func(w http.ResponseWriter, r *http.Request) {
		var in core.SearchSessionsInput
		if !decode(w, r, &in) {
			return
		}
		out, err := svc.SearchSessions(r.Context(), in)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	})

	mux.HandleFunc("POST /tools/list_events", func(w http.ResponseWriter, r *http.Request) {
		var in core.ListEventsInput
		if r.ContentLength > 0 && !decode(w, r, &in) {
			return
		}
		writeJSON(w, http.StatusOK, svc.ListEvents(r.Context()))
	})

	mux.HandleFunc("POST /tools/refresh_data", func(w http.ResponseWriter, r *http.Request) {
		var in core.RefreshDataInput
		if r.ContentLength > 0 && !decode(w, r, &in) {
			return
		}
		out, err := svc.RefreshData(r.Context(), in)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	})

	return mux
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Body == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty body"})
		return false
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return false
	}
	return true
}

// writeErr maps a service error to a status: an unknown event slug is the
// caller's mistake, anything else is an upstream/data failure.
func writeErr(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	if strings.HasPrefix(err.Error(), "unknown event") {
		status = http.StatusBadRequest
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// toolCatalog describes the tools for agent frameworks / documentation.
func toolCatalog() []map[string]any {
	return []map[string]any{
		{
			"name":        "find_sessions",
			"method":      "POST /tools/find_sessions",
			"description": "Exact structured filter over sessions. Use for precise queries like 'sessions from Google', 'sessions on Tuesday in Ballroom ACE'. Empty fields are ignored; all provided fields must match.",
			"input":       map[string]string{"company": "string", "speaker": "string", "day": "YYYY-MM-DD", "room": "string", "track": "string", "includeService": "bool", "limit": "int", "event": "string (optional, default event)"},
		},
		{
			"name":        "get_speaker",
			"method":      "POST /tools/get_speaker",
			"description": "Resolve a (possibly misspelled) speaker name to speakers and their sessions, ranked by confidence. Use for 'does Tim Hockin have any sessions'.",
			"input":       map[string]string{"name": "string (required)", "threshold": "float 0..1 (optional)", "event": "string (optional)"},
		},
		{
			"name":        "search_sessions",
			"method":      "POST /tools/search_sessions",
			"description": "Hybrid keyword + semantic search over session titles/abstracts. Use for fuzzy topic queries like 'sessions on service mesh' or 'anything about eBPF'.",
			"input":       map[string]string{"query": "string (required)", "k": "int (optional, default 10)", "event": "string (optional)"},
		},
		{
			"name":        "list_events",
			"method":      "POST /tools/list_events",
			"description": "List the conferences this server can answer questions about, with how fresh each one's data is. Also available as GET /events.",
			"input":       map[string]string{},
		},
		{
			"name":        "refresh_data",
			"method":      "POST /tools/refresh_data",
			"description": "Re-fetch a conference's schedule from Sessionize and reload it. Every response carries dataAsOf; only refresh when that is too stale for the question.",
			"input":       map[string]string{"event": "string (optional, default event)"},
		},
	}
}
