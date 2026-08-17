# hallway — conference schedule query service

*Named for the hallway track: the part of a conference where you actually find
the people and talks that matter.*

An **LLM-agnostic** query service over conference schedules (sourced from the
public Sessionize API). It exposes the same tools over **REST** and **MCP**, so
any agent — a web chatbot, a custom agent framework, or an MCP client like
Claude Desktop — can answer questions like:

- *"Show me all the sessions from Google"* → `find_sessions`
- *"Does Tim Hockin have any sessions?"* → `get_speaker` (tolerates typos)
- *"Are there any sessions on service mesh / eBPF / GPU scheduling?"* → `search_sessions`
- *"What conferences do you know about, and how current is this?"* → `list_events`
- *"Check whether that's changed"* → `refresh_data`

The service contains **no LLM**. The agent does the natural-language →
tool-call translation and calls these tools; the service returns precise,
grounded data.

## Design

```
core/        LLM-agnostic query engine (no HTTP/MCP/LLM knowledge)
  event.go   per-conference config + registry (the only event-specific data)
  model.go   resolved Session/Speaker types
  index.go   load all.json + speakers.json, join speakers↔sessions in memory
  fetch.go   pull the Sessionize API (in-memory fetch; atomic writes)
  store.go   per-event snapshots, on-demand loading, refresh + atomic swap
  fuzzy.go   name normalization + Levenshtein/token similarity
  speaker.go get_speaker: fuzzy name resolution
  find.go    find_sessions: exact structured filters
  search.go  search_sessions: BM25 + embeddings fused with Reciprocal Rank Fusion
  service.go façade with typed tool I/O (shared by both transports)
embed/       pluggable embeddings
  embed.go   Embedder interface + cosine
  gemini.go  Gemini text-embedding-004 client (AI Studio, API key)
  vertex.go  Vertex AI client (ADC, no API key)
transport/
  http.go    REST front door
  mcp.go     MCP front door (same core.Service)
cmd/report/  company-filtered markdown report generator
main.go      wiring + flags
```

Data (~400 sessions, ~530 speakers) is tiny, so everything lives in memory —
**no database, no vector store.** Semantic search is a linear cosine scan over a
slice of vectors; that's all a "vector store" needs to be at this scale.

### Why this shape, not a vector DB

Three of the target queries are *exact-match filters* (company, speaker name),
which embeddings get wrong. Only fuzzy *topic* search benefits from vectors —
and even there we fuse it with keyword BM25 so exact terms ("eBPF", "SPIFFE")
aren't lost to semantic drift. See the three fuzzy layers:

1. **Fuzzy field values** — normalization + substring (company/room).
2. **Fuzzy names** — Levenshtein + token-set similarity, ranked candidates
   (`get_speaker` returns "Tim Hockin" for `Tim Hokin`).
3. **Fuzzy topics** — hybrid BM25 + embedding cosine, fused with RRF
   (`search_sessions`).

## Events

Everything conference-specific lives in one place: the `Event` registry in
`core/event.go`. Nothing else in the codebase knows which conference it's
serving — session deep-links, report titles, and even the MCP tool descriptions
are all derived from the selected event.

Adding a new KubeCon (or any Sessionize-hosted conference) is one registry
entry:

```go
"kubecon-eu-2027": {
    Slug:           "kubecon-eu-2027",
    Name:           "KubeCon + CloudNativeCon Europe 2027",
    Location:       "Amsterdam, Netherlands",
    Dates:          "March 23–26, 2027",
    TZLabel:        "CET (UTC+1)",
    TZShort:        "CET",
    SessionizeCode: "<code>",
    ScheduleURL:    "https://events.linuxfoundation.org/kubecon-cloudnativecon-europe/program/schedule/?id=",
},
```

…or, with no recompile, the same thing in a JSON file passed to `-events`:

```json
[
  {
    "slug": "kubecon-eu-2027",
    "name": "KubeCon + CloudNativeCon Europe 2027",
    "location": "Amsterdam, Netherlands",
    "dates": "March 23–26, 2027",
    "tzLabel": "CET (UTC+1)",
    "tzShort": "CET",
    "sessionizeCode": "<code>",
    "scheduleUrl": "https://events.linuxfoundation.org/kubecon-cloudnativecon-europe/program/schedule/?id="
  }
]
```

Find the Sessionize code by viewing the event's schedule page source and looking
for the `sessionize.com/api/v2/<code>/view/...` embed.

**Serving several conferences at once.** One process handles all registered
events. `-event` picks the *default* — the one used when a tool call doesn't say
otherwise — and it's the only one loaded at startup. Every tool takes an
optional `event` slug; naming an event that isn't in memory loads it on demand
(from `<data-dir>/<slug>-all.json` if present, otherwise straight from the API)
and keeps it loaded. Agents discover what's available via `list_events`.

`-sessionize-code` overrides the default event's code without touching the
registry — useful for a one-off or an unregistered conference.

## Run

```bash
# install the released binary...
go install github.com/gke-demos/hallway@latest

# ...or build from a clone
git clone https://github.com/gke-demos/hallway && cd hallway
go build -o hallway .

# serve immediately: uses local all.json + speakers.json if they're there, and
# otherwise downloads them from Sessionize. No credentials needed either way —
# without them you get keyword/BM25 search instead of semantic.
./hallway                       # REST + MCP-over-HTTP on :8080
./hallway -event kubecon-na-2026 # pick the default conference (this is the default)

# refresh the local files from the Sessionize API first, then serve
./hallway -refresh

# ...and keep them fresh while running (see Data freshness)
./hallway -refresh -refresh-interval 1h

# semantic search via Gemini Developer API (AI Studio) — API key
GEMINI_API_KEY=... ./hallway

# semantic search via Vertex AI — Application Default Credentials (no API key)
GOOGLE_GENAI_USE_VERTEXAI=true \
GOOGLE_CLOUD_PROJECT=my-project \
GOOGLE_CLOUD_LOCATION=us-central1 \
  ./hallway

# as an MCP stdio server (e.g. for Claude Desktop)
./hallway -stdio
```

Either backend embeds sessions once and caches vectors to
`<cache-dir>/<event>.embeddings.json`.

Flags: `-addr` (listen addr), `-all`/`-speakers` (default event's data files),
`-data-dir` (where other events' data lives), `-cache-dir` (vector caches),
`-stdio` (MCP stdio mode), `-event` (default conference), `-events` (register
more), `-refresh` / `-refresh-interval` / `-sessionize-code` /
`-sessionize-url` (data freshness, below).

## REST API

| Method & path | Body | Purpose |
|---|---|---|
| `GET /health` | — | per-event counts, data age, semantic on/off |
| `GET /tools` | — | machine-readable tool catalog (for agent frameworks) |
| `GET /events` | — | same as `list_events` |
| `POST /tools/find_sessions` | `{company?, speaker?, day?, room?, track?, includeService?, limit?, event?}` | exact structured filter |
| `POST /tools/get_speaker` | `{name, threshold?, event?}` | fuzzy speaker → sessions |
| `POST /tools/search_sessions` | `{query, k?, event?}` | hybrid topic search |
| `POST /tools/list_events` | — | which conferences are served, and how fresh |
| `POST /tools/refresh_data` | `{event?}` | re-fetch one conference now |

```bash
curl -sX POST localhost:8080/tools/find_sessions   -d '{"company":"Google"}'
curl -sX POST localhost:8080/tools/get_speaker     -d '{"name":"Tim Hokin"}'
curl -sX POST localhost:8080/tools/search_sessions -d '{"query":"service mesh security","k":5}'
curl -sX POST localhost:8080/tools/refresh_data    -d '{}'
```

`event` is optional everywhere and defaults to `-event`. An unknown slug is a
`400` listing the known ones.

Every query response carries freshness metadata, so an agent can hedge or
refresh rather than confidently reciting last week's schedule:

```json
{
  "event": "kubecon-na-2026",
  "eventName": "KubeCon + CloudNativeCon North America 2026",
  "dataAsOf": "2026-08-17T12:27:58Z",
  "dataAgeHours": 0.2,
  "count": 38,
  "sessions": [ ... ]
}
```

## MCP

- **HTTP transport:** point an MCP client at `http://<host>:8080/mcp`.
- **stdio transport:** run `./hallway -stdio`. Example Claude Desktop config:

```json
{
  "mcpServers": {
    "hallway": {
      "command": "/path/to/hallway",
      "args": ["-stdio", "-all", "/path/to/all.json", "-speakers", "/path/to/speakers.json"],
      "env": { "GEMINI_API_KEY": "..." }
    }
  }
}
```

Tools exposed: `find_sessions`, `get_speaker`, `search_sessions`, `list_events`,
`refresh_data` (identical contracts to REST).

## Data freshness

A conference schedule moves — sessions get added, rooms change, talks are
cancelled — and an MCP stdio server launched by a desktop client can stay
running for days. So data is not a startup-only snapshot.

Each event's data lives in an immutable **snapshot** (index + search structures
+ an "as of" stamp). A refresh builds an entirely new snapshot alongside the
live one and swaps it in only once it's complete, so in-flight tool calls keep
serving consistent data and a failed refresh changes nothing.

Three ways data gets refreshed:

```bash
./hallway -refresh                        # once, at startup
./hallway -refresh-interval 1h            # in the background, forever
curl -sX POST localhost:8080/tools/refresh_data -d '{}'   # on demand (also an MCP tool)
```

| Flag | Default | Meaning |
|---|---|---|
| `-event` | `kubecon-na-2026` | Default conference (see [Events](#events)) |
| `-events` | *(none)* | JSON file registering additional conferences |
| `-refresh` | `false` | Fetch the default event at startup |
| `-refresh-interval` | `0` | Re-fetch every loaded event on this interval; `0` disables |
| `-sessionize-code` | *(empty)* | Override the default event's API code. **Non-empty implies `-refresh`.** |
| `-sessionize-url` | `https://sessionize.com/api/v2` | API base; endpoints are `{url}/{code}/view/{All,Speakers}` |
| `-data-dir` | `.` | Where non-default events' `<slug>-all.json` / `<slug>-speakers.json` live |

Failure behavior — stale data always beats no data:

- **Startup refresh fails, local files exist** → warns and serves the existing
  (stale) data rather than refusing to start.
- **Startup refresh fails, no local files** → fatal, with the underlying error.
- **Background or `refresh_data` fails** → logged (or returned as an error), and
  the previous snapshot keeps serving.
- **Writes are atomic** (temp file + rename, after both downloads succeed *and*
  validate as non-empty JSON), so a failed refresh never truncates good data.
  Caching to disk is best-effort: a read-only working directory produces a
  warning, not an outage.

**`refresh_data` is deliberately a last resort.** Every other tool already
reports `dataAsOf` and `dataAgeHours`, and the tool description tells the agent
to refresh only when that age is actually too old for the question — not
speculatively, and not in a retry loop. It is a network round-trip that also
re-embeds anything that changed.

**Why the interval is affordable.** Embedding vectors are cached per *document*,
keyed by a hash of that session's text. A refresh that adds two sessions embeds
two sessions, not all ~400 — so hourly refresh costs essentially nothing when
the schedule is quiet. Changing embedding provider or model invalidates the
whole cache, as it must.

Equivalent manual fetch, if you prefer curl:

```bash
curl -s https://sessionize.com/api/v2/svi82w6c/view/All      -o all.json
curl -s https://sessionize.com/api/v2/svi82w6c/view/Speakers -o speakers.json
```

## Company report (`cmd/report`)

Regenerates the company-filtered markdown (the report file): every session
with at least one speaker from the target company — listing *all* speakers on
those sessions — plus that company's speaker roster.

```bash
# refresh from the API, then write kubecon-na-2026-google-2026-08-17.md
go run ./cmd/report -refresh

# generate from local files, no network (needs a previous -refresh)
go run ./cmd/report

# a different company / event / output file
go run ./cmd/report -company Microsoft -out kubecon-na-2026-microsoft.md
go run ./cmd/report -event kubecon-eu-2027 -refresh -company Google
```

| Flag | Default | Meaning |
|---|---|---|
| `-company` | `Google` | Company to filter on (fuzzy, case-insensitive) |
| `-out` | `<event-slug>-<company-slug>-<date>.md` | Output path, e.g. `kubecon-na-2026-google-2026-08-17.md`. Dated so daily reports don't overwrite each other |
| `-event` | `kubecon-na-2026` | Which conference; supplies the title, city, dates, timezone and link base |
| `-all` / `-speakers` | `all.json` / `speakers.json` | Input files |
| `-refresh` / `-sessionize-code` / `-sessionize-url` | — | Same semantics as the server |

Times are rendered in the event's timezone (`TZShort` in the registry) — there
is no `-tz` flag; it follows the event.

Sessions are ordered by start time, preserving Sessionize's published order for
ties. Unlike the service, a failed refresh here is **fatal** — a report should
never silently be built from stale data.

## Embedding providers

Two Google backends ship in `embed/`, selected at startup:

| Backend | File | Auth | Selected when |
|---|---|---|---|
| Gemini Developer API (AI Studio) | `embed/gemini.go` | `GEMINI_API_KEY` | key is set and Vertex isn't requested |
| Vertex AI | `embed/vertex.go` | **Application Default Credentials (ADC)** — no API key | `GOOGLE_GENAI_USE_VERTEXAI=true` or `GOOGLE_CLOUD_PROJECT` is set |

Vertex takes precedence when requested; otherwise the AI Studio key is used;
otherwise the service runs keyword-only.

**ADC** is resolved automatically by `golang.org/x/oauth2/google` from, in order:
`GOOGLE_APPLICATION_CREDENTIALS` (service-account key file), `gcloud auth
application-default login` (local user creds), or the attached service
account / metadata server (on GCE, Cloud Run, GKE, etc.). Tokens auto-refresh.
Vertex env vars: `GOOGLE_CLOUD_PROJECT` (required), `GOOGLE_CLOUD_LOCATION`
(default `us-central1`, and `global` is supported), `VERTEX_EMBED_MODEL`
(default `text-embedding-004`).

Embedding requests are batched under both an instance count and a token budget,
because Vertex rejects a `:predict` call whose instances exceed the model's
total input token limit — for this corpus, a naive batch of 100 session
abstracts is roughly 23k tokens against a 20k cap.

To add another provider (OpenAI, Cohere, a local model), implement
`embed.Embedder` (three methods: `Embed`, `EmbedQuery`, `Name`) and pass it into
`core.NewSearcher`.

## License

Apache License 2.0 — see [LICENSE](LICENSE).

## Disclaimer

An independent, unofficial tool. Not affiliated with, endorsed by, or supported
by the Cloud Native Computing Foundation, the Linux Foundation, or Sessionize.
Schedule data is read from Sessionize's public API and belongs to the
respective conference organizers and speakers.
