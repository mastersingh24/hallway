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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultSessionizeBase is the public Sessionize read-only API root. The full
// endpoint is {base}/{code}/view/{view} — no authentication required.
const DefaultSessionizeBase = "https://sessionize.com/api/v2"

// Fetcher downloads Sessionize views and refreshes the local JSON files that
// Load reads. It is only used when an API code is supplied; otherwise the
// service runs entirely off the files already on disk.
type Fetcher struct {
	BaseURL string // e.g. https://sessionize.com/api/v2
	Code    string // event API code, e.g. svi82w6c
	HTTP    *http.Client
}

// NewFetcher returns a Fetcher for the given event code. base may be empty to
// use DefaultSessionizeBase.
func NewFetcher(base, code string) *Fetcher {
	if base == "" {
		base = DefaultSessionizeBase
	}
	return &Fetcher{
		BaseURL: strings.TrimRight(base, "/"),
		Code:    code,
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}
}

// viewURL builds the endpoint for a named view (All, Speakers, Sessions, ...).
func (f *Fetcher) viewURL(view string) string {
	return fmt.Sprintf("%s/%s/view/%s", f.BaseURL, f.Code, view)
}

// Data is a validated pair of Sessionize payloads, not yet written anywhere.
// Keeping fetch and write separate lets long-running servers refresh in memory
// even when the working directory isn't writable (e.g. an MCP stdio server
// launched by a desktop client).
type Data struct {
	All      []byte
	Speakers []byte
}

// Fetch downloads the All and Speakers views and validates both before
// returning. Nothing is written to disk.
func (f *Fetcher) Fetch(ctx context.Context) (Data, error) {
	allBytes, err := f.get(ctx, "All")
	if err != nil {
		return Data{}, err
	}
	spkBytes, err := f.get(ctx, "Speakers")
	if err != nil {
		return Data{}, err
	}

	// Validate shape before the caller does anything irreversible with it.
	var probeAll rawAll
	if err := json.Unmarshal(allBytes, &probeAll); err != nil {
		return Data{}, fmt.Errorf("sessionize All: unexpected payload: %w", err)
	}
	if len(probeAll.Sessions) == 0 {
		return Data{}, fmt.Errorf("sessionize All: response contained no sessions (wrong code %q?)", f.Code)
	}
	var probeSpeakers []rawSpeaker
	if err := json.Unmarshal(spkBytes, &probeSpeakers); err != nil {
		return Data{}, fmt.Errorf("sessionize Speakers: unexpected payload: %w", err)
	}
	if len(probeSpeakers) == 0 {
		return Data{}, fmt.Errorf("sessionize Speakers: response contained no speakers (wrong code %q?)", f.Code)
	}

	return Data{All: allBytes, Speakers: spkBytes}, nil
}

// Write persists the payloads atomically (temp file + rename), so a reader
// never observes a half-written file and a failed write never truncates good
// local data.
func (d Data) Write(allPath, speakersPath string) error {
	if err := writeFileAtomic(allPath, d.All); err != nil {
		return err
	}
	return writeFileAtomic(speakersPath, d.Speakers)
}

// Refresh downloads both views and writes them to allPath and speakersPath,
// only after every download has succeeded and validated as JSON.
func (f *Fetcher) Refresh(ctx context.Context, allPath, speakersPath string) error {
	d, err := f.Fetch(ctx)
	if err != nil {
		return err
	}
	return d.Write(allPath, speakersPath)
}

func (f *Fetcher) get(ctx context.Context, view string) ([]byte, error) {
	url := f.viewURL(view)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	client := f.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: read body: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: status %d: %s", url, resp.StatusCode, errSnippet(body))
	}
	return body, nil
}

// errSnippet renders an error body as one short line. Sessionize returns an
// HTML page for a bad event code, so collapse whitespace and don't spew markup
// into the log.
func errSnippet(body []byte) string {
	s := strings.Join(strings.Fields(string(body)), " ")
	if strings.HasPrefix(s, "<") {
		return "(HTML error page — check the event code)"
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	if s == "" {
		s = "(empty response)"
	}
	return s
}

// writeFileAtomic writes via a temp file + rename so readers never observe a
// partially written file.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	// CreateTemp makes 0600; use normal data-file perms instead.
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
