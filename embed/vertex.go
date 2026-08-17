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
	"net/http"
	"os"
	"time"
	"unicode/utf8"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// cloudPlatformScope is the OAuth2 scope Vertex AI requires.
const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

// Vertex implements Embedder using Gemini embedding models on Vertex AI,
// authenticated with Application Default Credentials (ADC). No API key: ADC
// resolves credentials from GOOGLE_APPLICATION_CREDENTIALS, gcloud user login,
// or the workload's attached service account / metadata server automatically.
type Vertex struct {
	Project  string
	Location string
	Model    string
	HTTP     *http.Client // an oauth2 client that injects ADC bearer tokens
}

// NewVertexFromEnv builds a Vertex embedder when Vertex is requested via env:
//
//	GOOGLE_GENAI_USE_VERTEXAI=true   (or GOOGLE_CLOUD_PROJECT being set)
//	GOOGLE_CLOUD_PROJECT=<project>   (required)
//	GOOGLE_CLOUD_LOCATION=<region>   (optional, default "us-central1")
//	VERTEX_EMBED_MODEL=<model>       (optional, default "text-embedding-004")
//
// It returns (nil, false, nil) when Vertex isn't requested, and (nil, false, err)
// when it's requested but misconfigured (e.g. ADC unavailable), so callers can
// surface the problem instead of silently falling back.
func NewVertexFromEnv(ctx context.Context) (*Vertex, bool, error) {
	useVertex := os.Getenv("GOOGLE_GENAI_USE_VERTEXAI") == "true" || os.Getenv("GOOGLE_CLOUD_PROJECT") != ""
	if !useVertex {
		return nil, false, nil
	}

	project := os.Getenv("GOOGLE_CLOUD_PROJECT")
	if project == "" {
		return nil, false, fmt.Errorf("vertex: GOOGLE_CLOUD_PROJECT must be set")
	}
	location := os.Getenv("GOOGLE_CLOUD_LOCATION")
	if location == "" {
		location = "us-central1"
	}
	model := os.Getenv("VERTEX_EMBED_MODEL")
	if model == "" {
		model = "text-embedding-004"
	}

	// ADC: resolves GOOGLE_APPLICATION_CREDENTIALS, gcloud ADC, or the
	// metadata server, and auto-refreshes the token.
	ts, err := google.DefaultTokenSource(ctx, cloudPlatformScope)
	if err != nil {
		return nil, false, fmt.Errorf("vertex: application default credentials: %w", err)
	}
	client := oauth2.NewClient(ctx, ts)
	client.Timeout = 60 * time.Second

	return &Vertex{
		Project:  project,
		Location: location,
		Model:    model,
		HTTP:     client,
	}, true, nil
}

func (v *Vertex) Name() string { return "vertex/" + v.Model }

// endpoint returns the API host for a location. Regional locations use a
// {location}-prefixed host, but "global" is served by the unprefixed host —
// "global-aiplatform.googleapis.com" does not exist, and requests to it come
// back as a generic Google 404 HTML page rather than an API error.
func (v *Vertex) endpoint() string {
	if v.Location == "global" {
		return "https://aiplatform.googleapis.com"
	}
	return "https://" + v.Location + "-aiplatform.googleapis.com"
}

// Batching limits for a single :predict call. Vertex bounds a request by total
// input tokens as well as instance count — 100 session descriptions is well
// under the instance cap but blows straight past the 20k token budget — so
// chunking by count alone is not enough. There is no local tokenizer, so
// estimate from characters and leave generous headroom.
const (
	maxInstances     = 100
	maxChunkTokens   = 15000
	charsPerToken    = 4
	maxInstanceChars = 2048 * charsPerToken // the models truncate past 2048 tokens anyway
)

func estimateTokens(s string) int {
	return (len(s) + charsPerToken - 1) / charsPerToken
}

// clampInstance keeps one very long document from eating a whole request's
// token budget. The embedding models truncate beyond their per-input limit
// regardless, so this only moves that truncation client-side.
func clampInstance(s string) string {
	if len(s) <= maxInstanceChars {
		return s
	}
	b := []byte(s)[:maxInstanceChars]
	for len(b) > 0 && !utf8.Valid(b) { // don't split a multi-byte rune
		b = b[:len(b)-1]
	}
	return string(b)
}

// chunkEnd returns the exclusive end of the batch starting at start, bounded by
// both the instance count and the estimated token budget. It always advances by
// at least one instance so an oversized document can't stall the loop.
func chunkEnd(texts []string, start int) int {
	tokens := 0
	end := start
	for end < len(texts) && end-start < maxInstances {
		t := estimateTokens(texts[end])
		if end > start && tokens+t > maxChunkTokens {
			break
		}
		tokens += t
		end++
	}
	return end
}

// ---- wire shapes (Vertex predict API) ----

type vertexInstance struct {
	Content  string `json:"content"`
	TaskType string `json:"task_type,omitempty"`
}
type vertexPredictReq struct {
	Instances []vertexInstance `json:"instances"`
}
type vertexPredictResp struct {
	Predictions []struct {
		Embeddings struct {
			Values []float32 `json:"values"`
		} `json:"embeddings"`
	} `json:"predictions"`
}

// Embed embeds documents using the RETRIEVAL_DOCUMENT task type.
func (v *Vertex) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return v.predict(ctx, texts, "RETRIEVAL_DOCUMENT")
}

// EmbedQuery embeds a single query using the RETRIEVAL_QUERY task type.
func (v *Vertex) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	out, err := v.predict(ctx, []string{text}, "RETRIEVAL_QUERY")
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("vertex: empty embedding response")
	}
	return out[0], nil
}

func (v *Vertex) predict(ctx context.Context, texts []string, taskType string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	url := fmt.Sprintf("%s/v1/projects/%s/locations/%s/publishers/google/models/%s:predict",
		v.endpoint(), v.Project, v.Location, v.Model)

	var all [][]float32
	for start := 0; start < len(texts); {
		end := chunkEnd(texts, start)
		reqBody := vertexPredictReq{}
		for _, t := range texts[start:end] {
			reqBody.Instances = append(reqBody.Instances, vertexInstance{Content: clampInstance(t), TaskType: taskType})
		}
		start = end
		buf, err := json.Marshal(reqBody)
		if err != nil {
			return nil, err
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")

		resp, err := v.HTTP.Do(httpReq) // oauth2 client injects the ADC bearer token
		if err != nil {
			return nil, err
		}
		body, _ := readAllClose(resp)
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("vertex embed: status %d: %s", resp.StatusCode, errSnippet(body))
		}
		var parsed vertexPredictResp
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, fmt.Errorf("vertex embed: decode: %w", err)
		}
		for _, p := range parsed.Predictions {
			all = append(all, p.Embeddings.Values)
		}
	}
	if len(all) != len(texts) {
		return nil, fmt.Errorf("vertex embed: expected %d vectors, got %d", len(texts), len(all))
	}
	return all, nil
}
