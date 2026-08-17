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

package transport

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gke-demos/hallway/core"
)

// NewMCPServer builds an MCP server exposing the same tools as REST. Handlers
// return a typed output value; the SDK serializes it to structured tool content
// automatically (Content left nil).
//
// Handlers close over the Service, not over a snapshot, so they always read
// whatever data is current — a background or agent-triggered refresh is picked
// up by the next tool call without restarting the server.
func NewMCPServer(svc *core.Service) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "hallway",
		Version: "0.2.0",
	}, nil)

	// Name the default event concretely in the descriptions — it measurably
	// helps tool selection — while making the event parameter discoverable.
	def := svc.Default
	if ev, err := core.LookupEvent(def); err == nil {
		def = ev.Name
	}
	scope := fmt.Sprintf("Defaults to %s; pass 'event' for another conference "+
		"(known: %s).", def, strings.Join(core.EventSlugs(), ", "))

	mcp.AddTool(server, &mcp.Tool{
		Name: "find_sessions",
		Description: "Exact structured filter over conference sessions. " +
			"Use for precise queries like 'sessions from Google' or 'sessions on Tuesday'. " +
			"Empty fields are ignored; all provided fields must match (AND). " + scope,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in core.FindSessionsInput) (*mcp.CallToolResult, core.FindSessionsOutput, error) {
		out, err := svc.FindSessions(ctx, in)
		if err != nil {
			return nil, core.FindSessionsOutput{}, err
		}
		return nil, out, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "get_speaker",
		Description: "Resolve a (possibly misspelled) speaker name to speakers and their sessions, " +
			"ranked by confidence. Use for 'does Tim Hockin have any sessions'. " + scope,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in core.GetSpeakerInput) (*mcp.CallToolResult, core.GetSpeakerOutput, error) {
		out, err := svc.GetSpeaker(ctx, in)
		if err != nil {
			return nil, core.GetSpeakerOutput{}, err
		}
		return nil, out, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "search_sessions",
		Description: "Hybrid keyword + semantic search over session titles/abstracts. " +
			"Use for fuzzy topic queries like 'sessions on service mesh' or 'anything about eBPF'. " + scope,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in core.SearchSessionsInput) (*mcp.CallToolResult, core.SearchSessionsOutput, error) {
		out, err := svc.SearchSessions(ctx, in)
		if err != nil {
			return nil, core.SearchSessionsOutput{}, err
		}
		return nil, out, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "list_events",
		Description: "List the conferences this server can answer questions about, with how fresh " +
			"each one's data is. Call this first when the user mentions a conference other than " +
			"the default, or to check data freshness before answering a time-sensitive question.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in core.ListEventsInput) (*mcp.CallToolResult, core.ListEventsOutput, error) {
		return nil, svc.ListEvents(ctx), nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "refresh_data",
		Description: "Re-fetch a conference's schedule from the upstream Sessionize API and reload it. " +
			"Every other tool already reports 'dataAsOf' — only call this when that data is too old " +
			"for the question (e.g. the user asks what changed, or is at the event and needs live " +
			"room/time details). Do not call it speculatively or in a retry loop: it is a network " +
			"round-trip and re-embeds any changed sessions.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in core.RefreshDataInput) (*mcp.CallToolResult, core.RefreshDataOutput, error) {
		out, err := svc.RefreshData(ctx, in)
		if err != nil {
			return nil, core.RefreshDataOutput{}, err
		}
		return nil, out, nil
	})

	return server
}

// ServeMCPStdio runs the MCP server over stdio (for Claude Desktop etc.).
func ServeMCPStdio(ctx context.Context, svc *core.Service) error {
	return NewMCPServer(svc).Run(ctx, &mcp.StdioTransport{})
}

// MCPHTTPHandler returns a streamable-HTTP handler so MCP clients can connect
// over HTTP (mount it alongside the REST routes, e.g. at /mcp).
func MCPHTTPHandler(svc *core.Service) http.Handler {
	server := NewMCPServer(svc)
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, nil)
}
