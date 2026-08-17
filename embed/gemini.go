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

package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// GeminiModel is the default embedding model.
const GeminiModel = "text-embedding-004"

const geminiBase = "https://generativelanguage.googleapis.com/v1beta/models/"

// Gemini implements Embedder using Google's Generative Language API.
type Gemini struct {
	APIKey string
	Model  string
	HTTP   *http.Client
}

// NewGeminiFromEnv returns a Gemini embedder if GEMINI_API_KEY is set, else
// (nil, false) so callers can fall back to keyword-only search.
func NewGeminiFromEnv() (*Gemini, bool) {
	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		return nil, false
	}
	return &Gemini{
		APIKey: key,
		Model:  GeminiModel,
		HTTP:   &http.Client{Timeout: 60 * time.Second},
	}, true
}

func (g *Gemini) model() string {
	if g.Model != "" {
		return g.Model
	}
	return GeminiModel
}

func (g *Gemini) Name() string { return "gemini/" + g.model() }

// ---- wire shapes ----

type gemContent struct {
	Parts []gemPart `json:"parts"`
}
type gemPart struct {
	Text string `json:"text"`
}
type gemEmbedReq struct {
	Model    string     `json:"model"`
	Content  gemContent `json:"content"`
	TaskType string     `json:"taskType,omitempty"`
}
type gemBatchReq struct {
	Requests []gemEmbedReq `json:"requests"`
}
type gemBatchResp struct {
	Embeddings []struct {
		Values []float32 `json:"values"`
	} `json:"embeddings"`
}

// Embed embeds documents using RETRIEVAL_DOCUMENT task type.
func (g *Gemini) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return g.batch(ctx, texts, "RETRIEVAL_DOCUMENT")
}

// EmbedQuery embeds a single query using RETRIEVAL_QUERY task type.
func (g *Gemini) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	out, err := g.batch(ctx, []string{text}, "RETRIEVAL_QUERY")
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("gemini: empty embedding response")
	}
	return out[0], nil
}

func (g *Gemini) batch(ctx context.Context, texts []string, taskType string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	modelPath := "models/" + g.model()

	// Gemini caps batchEmbedContents at 100 requests per call; chunk to be safe.
	const chunk = 100
	var all [][]float32
	for start := 0; start < len(texts); start += chunk {
		end := start + chunk
		if end > len(texts) {
			end = len(texts)
		}
		reqBody := gemBatchReq{}
		for _, t := range texts[start:end] {
			reqBody.Requests = append(reqBody.Requests, gemEmbedReq{
				Model:    modelPath,
				Content:  gemContent{Parts: []gemPart{{Text: t}}},
				TaskType: taskType,
			})
		}
		buf, err := json.Marshal(reqBody)
		if err != nil {
			return nil, err
		}
		url := fmt.Sprintf("%s%s:batchEmbedContents?key=%s", geminiBase, modelPath, g.APIKey)
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")

		client := g.HTTP
		if client == nil {
			client = http.DefaultClient
		}
		resp, err := client.Do(httpReq)
		if err != nil {
			return nil, err
		}
		body, _ := readAllClose(resp)
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("gemini embed: status %d: %s", resp.StatusCode, errSnippet(body))
		}
		var parsed gemBatchResp
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, fmt.Errorf("gemini embed: decode: %w", err)
		}
		for _, e := range parsed.Embeddings {
			all = append(all, e.Values)
		}
	}
	if len(all) != len(texts) {
		return nil, fmt.Errorf("gemini embed: expected %d vectors, got %d", len(texts), len(all))
	}
	return all, nil
}

func readAllClose(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// errSnippet renders an error body as one short line. A wrong host or a
// misrouted request returns a full HTML error page, and dumping that into the
// log buries the actual problem. Prefer the API's structured error message
// when there is one.
func errSnippet(body []byte) string {
	var parsed struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &parsed) == nil && parsed.Error.Message != "" {
		msg := parsed.Error.Message
		if parsed.Error.Status != "" {
			msg = parsed.Error.Status + ": " + msg
		}
		return truncate(msg, 300)
	}
	s := strings.Join(strings.Fields(string(body)), " ")
	if strings.HasPrefix(s, "<") {
		return "(HTML error page — check the project, location and model)"
	}
	if s == "" {
		return "(empty response)"
	}
	return truncate(s, 300)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
