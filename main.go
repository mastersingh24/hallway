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

// Command hallway serves a conference schedule as an LLM-agnostic query service
// over both REST and MCP. Any agent (Claude or otherwise) does the
// natural-language -> tool-call translation and calls these tools; the service
// itself contains no LLM.
//
// Named for the hallway track — the part of a conference where you actually
// find the people and talks that matter.
//
// Usage:
//
//	hallway                              # serve from local all.json + speakers.json
//	hallway -refresh                     # refresh those files from the API, then serve
//	hallway -refresh-interval 1h         # keep refreshing in the background
//	hallway -event kubecon-eu-2027       # pick a different default conference
//	hallway -events events.json          # register extra conferences
//	hallway -addr :9090                  # custom listen address
//	hallway -stdio                       # run as an MCP stdio server (for Claude Desktop)
//	GEMINI_API_KEY=... hallway           # enable semantic search (else keyword-only)
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/gke-demos/hallway/core"
	"github.com/gke-demos/hallway/embed"
	"github.com/gke-demos/hallway/transport"
)

func main() {
	var (
		allPath  = flag.String("all", "all.json", "path to the default event's Sessionize /All export")
		spkPath  = flag.String("speakers", "speakers.json", "path to the default event's Sessionize /Speakers export")
		dataDir  = flag.String("data-dir", ".", "directory holding <event>-all.json / <event>-speakers.json for non-default events")
		cacheDir = flag.String("cache-dir", ".", "directory for per-event embedding caches (empty disables caching)")
		addr     = flag.String("addr", ":8080", "listen address for REST + MCP-over-HTTP")
		stdio    = flag.Bool("stdio", false, "run as an MCP stdio server instead of HTTP")

		eventSlug = flag.String("event", core.DefaultEventSlug,
			"default conference; known: "+strings.Join(core.EventSlugs(), ", "))
		eventsFile = flag.String("events", "",
			"JSON file of additional conferences to register")
		refresh = flag.Bool("refresh", false,
			"refresh the default event from the Sessionize API at startup")
		interval = flag.Duration("refresh-interval", 0,
			"re-fetch loaded events on this interval (e.g. 1h); 0 disables")
		code = flag.String("sessionize-code", "",
			"override the default event's Sessionize API code (implies -refresh)")
		base = flag.String("sessionize-url", core.DefaultSessionizeBase,
			"Sessionize API base URL (used only when fetching)")
	)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if *eventsFile != "" {
		n, err := core.LoadEventsFile(*eventsFile)
		if err != nil {
			log.Fatalf("%v", err)
		}
		log.Printf("registered %d event(s) from %s", n, *eventsFile)
	}

	event, err := core.LookupEvent(*eventSlug)
	if err != nil {
		log.Fatalf("%v", err)
	}
	if *code != "" {
		event.SessionizeCode = *code
		if err := core.RegisterEvent(event); err != nil {
			log.Fatalf("%v", err)
		}
	}
	log.Printf("default event: %s (%s, code=%s)", event.Name, event.Slug, event.SessionizeCode)

	var emb embed.Embedder
	if v, requested, err := embed.NewVertexFromEnv(ctx); requested {
		if err != nil {
			log.Fatalf("vertex embeddings: %v", err)
		}
		emb = v
		log.Printf("semantic search: enabled via Vertex AI + ADC (%s, project=%s, location=%s)",
			v.Name(), v.Project, v.Location)
	} else if g, ok := embed.NewGeminiFromEnv(); ok {
		emb = g
		log.Printf("semantic search: enabled via Gemini Developer API (%s)", g.Name())
	} else {
		log.Printf("semantic search: disabled — set GEMINI_API_KEY (AI Studio) or " +
			"GOOGLE_GENAI_USE_VERTEXAI=true + GOOGLE_CLOUD_PROJECT (Vertex/ADC); using keyword/BM25 only")
	}

	store := core.NewStore(core.StoreConfig{
		Dir:             *dataDir,
		CacheDir:        *cacheDir,
		SessionizeURL:   *base,
		Embedder:        emb,
		Primary:         event.Slug,
		PrimaryAll:      *allPath,
		PrimarySpeakers: *spkPath,
	})
	svc := &core.Service{Store: store, Default: event.Slug}

	// Load the default event eagerly so startup fails loudly on bad data rather
	// than on the first tool call. Other events load on demand.
	if *refresh || *code != "" {
		log.Printf("refreshing %s from Sessionize (%s)", event.Slug, *base)
		if _, err := store.Refresh(ctx, event.Slug); err != nil {
			// A refresh failure is not fatal when usable local data exists —
			// serving slightly stale data beats not serving at all.
			log.Printf("WARNING: %v", err)
		}
	}
	snap, err := store.Get(ctx, event.Slug)
	if err != nil {
		log.Fatalf("load data: %v (pass -refresh to download the data first)", err)
	}
	log.Printf("loaded %d sessions, %d speakers (%s, as of %s)",
		len(snap.Index.SessionList), len(snap.Index.SpeakerList),
		snap.Source, snap.FetchedAt.Format(time.RFC3339))

	store.StartAutoRefresh(ctx, *interval)

	// MCP stdio mode: single transport, no HTTP.
	if *stdio {
		log.Printf("serving MCP over stdio")
		if err := transport.ServeMCPStdio(ctx, svc); err != nil {
			log.Fatalf("mcp stdio: %v", err)
		}
		return
	}

	// HTTP mode: REST at / and MCP (streamable HTTP) at /mcp.
	mux := http.NewServeMux()
	mux.Handle("/mcp", transport.MCPHTTPHandler(svc))
	mux.Handle("/mcp/", transport.MCPHTTPHandler(svc))
	mux.Handle("/", transport.RESTHandler(svc))

	srv := &http.Server{Addr: *addr, Handler: mux}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()

	log.Printf("REST + MCP-over-HTTP listening on %s (REST: /tools/*, MCP: /mcp)", *addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http server: %v", err)
	}
}
