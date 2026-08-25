// Package agent provides a thin wrapper around Google's Gemini SDK
// (google.golang.org/genai) for the Phase 2 agent layer. It deliberately
// exposes only two operations - construction and one-shot JSON generation -
// so the reconciliation engine's agent tier stays decoupled from any
// particular LLM vendor SDK.
package agent

import (
	"context"
	"fmt"

	"google.golang.org/genai"
)

// ModelName is the Gemini model used for all agent calls. flash-lite keeps
// per-decision latency and cost low enough to run across a full batch of
// exception records.
const ModelName = "gemini-flash-lite-latest"

// GeminiClient wraps the official genai SDK client behind a minimal surface.
type GeminiClient struct {
	client *genai.Client
}

// NewGeminiClient initializes a Gemini API client using the provided API key.
// The key is accepted as an explicit parameter (never read from the
// environment here) so callers own configuration and secrets management.
func NewGeminiClient(ctx context.Context, apiKey string) (*GeminiClient, error) {
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to initialize gemini client: %w", err)
	}
	return &GeminiClient{client: client}, nil
}

// GenerateJSON performs a single-turn generation constrained to JSON output:
// systemInstruction sets the behavioral contract (e.g. "you are a bank
// reconciliation judge..."), userPrompt carries the record-specific payload,
// and ResponseMIMEType makes the model emit parseable JSON rather than prose.
// The raw JSON string is returned; decoding into domain types is left to the
// caller so this wrapper never imports reconciliation models.
func (g *GeminiClient) GenerateJSON(ctx context.Context, systemInstruction string, userPrompt string) (string, error) {
	result, err := g.client.Models.GenerateContent(ctx, ModelName,
		genai.Text(userPrompt),
		&genai.GenerateContentConfig{
			SystemInstruction: genai.NewContentFromText(systemInstruction, genai.RoleUser),
			ResponseMIMEType:  "application/json",
		},
	)
	if err != nil {
		return "", fmt.Errorf("gemini generate content call failed: %w", err)
	}

	if len(result.Candidates) == 0 || result.Candidates[0] == nil ||
		len(result.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("gemini response contained no candidates or empty content")
	}

	text := result.Candidates[0].Content.Parts[0].Text
	if text == "" {
		return "", fmt.Errorf("gemini response text part was empty")
	}
	return text, nil
}
