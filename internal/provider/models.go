package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// FetchedModel is the raw per-model capability data reported by an
// OpenAI-compatible /models endpoint. OpenRouter reports all of it; most
// other providers report only a fraction — zero values mean "not reported",
// and the caller applies its own defaults.
type FetchedModel struct {
	ID               string
	ContextLength    int64    // tokens; 0 = not reported
	InputModalities  []string // nil = not reported
	OutputModalities []string // nil = not reported
	ReasoningEfforts []string // nil = not reported
	ReasoningDefault string   // "" = not reported
}

// fetchTimeout bounds the /models request.
const fetchTimeout = 15 * time.Second

// maxModelsBodyBytes caps the /models response body: a broken or hostile
// endpoint must not be able to force arbitrary amounts of memory into a
// single setup request. OpenRouter's full list is a couple of MB; 10 MiB
// leaves ample headroom.
const maxModelsBodyBytes = 10 << 20

// FetchModels pulls the provider's /models list and normalizes the fields
// needed for per-model metadata. It is intentionally tolerant: providers
// with a minimal response shape (just id/created/owned_by) yield entries
// with everything unset rather than an error.
func FetchModels(ctx context.Context, baseURL, apiKey string) ([]FetchedModel, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("provider base URL is empty")
	}
	url := strings.TrimSuffix(baseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := (&http.Client{Timeout: fetchTimeout}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models endpoint: HTTP %d", resp.StatusCode)
	}
	var body struct {
		Data []struct {
			ID            string `json:"id"`
			ContextLength int64  `json:"context_length"`
			Architecture  *struct {
				InputModalities  []string `json:"input_modalities"`
				OutputModalities []string `json:"output_modalities"`
			} `json:"architecture"`
			Reasoning *struct {
				SupportedEfforts []string `json:"supported_efforts"`
				DefaultEffort    string   `json:"default_effort"`
			} `json:"reasoning"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxModelsBodyBytes)).Decode(&body); err != nil {
		return nil, fmt.Errorf("models endpoint: %w", err)
	}
	out := make([]FetchedModel, 0, len(body.Data))
	for _, m := range body.Data {
		if m.ID == "" {
			continue
		}
		f := FetchedModel{ID: m.ID, ContextLength: m.ContextLength}
		if m.Architecture != nil {
			f.InputModalities = m.Architecture.InputModalities
			f.OutputModalities = m.Architecture.OutputModalities
		}
		if m.Reasoning != nil {
			f.ReasoningEfforts = m.Reasoning.SupportedEfforts
			f.ReasoningDefault = m.Reasoning.DefaultEffort
		}
		out = append(out, f)
	}
	return out, nil
}
