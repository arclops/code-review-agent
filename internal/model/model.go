// Package model talks to language models, and knows how to ask one for a review
// it can be merged with other reviews.
//
// The provider is a detail of the configuration rather than of the pipeline, so
// the same review runs against a commercial API, a model on the local machine,
// or nothing at all.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/arclops/code-review-agent/internal/config"
	"github.com/arclops/code-review-agent/internal/httpx"
)

// Issue is one thing a review found.
type Issue struct {
	Severity string `json:"severity"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Title    string `json:"title"`
	Detail   string `json:"detail"`
}

// Review is what a model returned for one file.
type Review struct {
	Summary     string   `json:"summary"`
	Issues      []Issue  `json:"issues"`
	Suggestions []string `json:"suggestions"`
	Verdict     string   `json:"verdict"`
}

// Finding is a deterministic result from a linter, handed to the model as a
// fact rather than as something to have an opinion about.
type Finding struct {
	File     string
	Line     int
	Rule     string
	Message  string
	Severity string
	Tool     string
}

// Request is one file to review, with everything the model is allowed to see.
type Request struct {
	Repository  string
	Number      int
	Title       string
	Description string
	Author      string
	File        string
	Language    string
	Patch       string
	Findings    []Finding
	Guidelines  []string
}

// Usage records what a call cost and how long it took, which is the comparison
// the challenge asks for when it says to try several providers.
type Usage struct {
	Model            string
	PromptTokens     int
	CompletionTokens int
	Duration         time.Duration
}

// Provider reviews one file.
type Provider interface {
	Name() string
	Review(ctx context.Context, request Request) (Review, Usage, error)
}

// None is the provider used when no model is configured. Everything
// deterministic still runs; the model simply has nothing to add.
type None struct{}

// Name implements Provider.
func (None) Name() string { return "none" }

// Review implements Provider.
func (None) Review(context.Context, Request) (Review, Usage, error) {
	return Review{Verdict: "no model configured"}, Usage{Model: "none"}, nil
}

// New builds the provider named in the configuration.
func New(cfg config.Config, lookup func(string) string) (Provider, error) {
	switch cfg.Model.Provider {
	case "", "none":
		return None{}, nil
	case "openai", "ollama":
		return &OpenAI{
			BaseURL:   cfg.Model.BaseURL,
			APIKey:    lookup(cfg.Model.APIKeyEnv),
			Model:     cfg.Model.Name,
			MaxTokens: cfg.Model.MaxTokens,
			Timeout:   cfg.Model.Timeout.Duration(),
			HTTP:      &http.Client{},
			Policy:    httpx.DefaultPolicy(),
		}, nil
	case "anthropic":
		return &Anthropic{
			BaseURL:   cfg.Model.BaseURL,
			APIKey:    lookup(cfg.Model.APIKeyEnv),
			Model:     cfg.Model.Name,
			MaxTokens: cfg.Model.MaxTokens,
			Timeout:   cfg.Model.Timeout.Duration(),
			HTTP:      &http.Client{},
			Policy:    httpx.DefaultPolicy(),
		}, nil
	default:
		return nil, fmt.Errorf("model: unknown provider %q", cfg.Model.Provider)
	}
}

// OpenAI speaks the chat completions API, which OpenAI, Ollama, DeepSeek,
// Groq, Together and most other hosts all expose. Swapping between them is a
// change of base URL and model name, which is the point the challenge makes.
type OpenAI struct {
	BaseURL   string
	APIKey    string
	Model     string
	MaxTokens int
	Timeout   time.Duration
	HTTP      *http.Client
	Policy    httpx.Policy
}

// Name implements Provider.
func (c *OpenAI) Name() string { return "openai-compatible/" + c.Model }

// Review implements Provider.
func (c *OpenAI) Review(ctx context.Context, request Request) (Review, Usage, error) {
	if c.BaseURL == "" {
		return Review{}, Usage{}, fmt.Errorf("model: no base URL configured for the openai provider")
	}
	system, user := Prompt(request)

	payload := map[string]any{
		"model": c.Model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		"temperature": 0,
		// Asking for JSON is what makes several per file reviews mergeable.
		"response_format": map[string]string{"type": "json_object"},
	}
	if c.MaxTokens > 0 {
		payload["max_tokens"] = c.MaxTokens
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return Review{}, Usage{}, err
	}

	started := time.Now()
	body, err := c.post(ctx, strings.TrimSuffix(c.BaseURL, "/")+"/chat/completions", encoded, map[string]string{
		"Authorization": "Bearer " + c.APIKey,
	})
	if err != nil {
		return Review{}, Usage{}, err
	}
	duration := time.Since(started)

	var response struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return Review{}, Usage{}, fmt.Errorf("model: could not decode the reply: %w", err)
	}
	if len(response.Choices) == 0 {
		return Review{}, Usage{}, fmt.Errorf("model: the reply had no choices: %s", truncate(body, 400))
	}

	review, err := ParseReview(response.Choices[0].Message.Content)
	usage := Usage{
		Model:            c.Model,
		PromptTokens:     response.Usage.PromptTokens,
		CompletionTokens: response.Usage.CompletionTokens,
		Duration:         duration,
	}
	return review, usage, err
}

// Anthropic speaks the messages API. Its request shape and its authentication
// header are different enough to be worth their own client, and being able to
// move between it and an OpenAI shaped host without touching the pipeline is
// the point.
type Anthropic struct {
	BaseURL   string
	APIKey    string
	Model     string
	MaxTokens int
	Timeout   time.Duration
	HTTP      *http.Client
	Policy    httpx.Policy
}

// Name implements Provider.
func (c *Anthropic) Name() string { return "anthropic/" + c.Model }

// Review implements Provider.
func (c *Anthropic) Review(ctx context.Context, request Request) (Review, Usage, error) {
	baseURL := c.BaseURL
	if baseURL == "" {
		baseURL = "https://api.anthropic.com/v1"
	}
	system, user := Prompt(request)

	maxTokens := c.MaxTokens
	if maxTokens == 0 {
		maxTokens = 1500
	}
	encoded, err := json.Marshal(map[string]any{
		"model":      c.Model,
		"max_tokens": maxTokens,
		"system":     system,
		"messages":   []map[string]string{{"role": "user", "content": user}},
	})
	if err != nil {
		return Review{}, Usage{}, err
	}

	started := time.Now()
	body, err := c.post(ctx, strings.TrimSuffix(baseURL, "/")+"/messages", encoded, map[string]string{
		"x-api-key":         c.APIKey,
		"anthropic-version": "2023-06-01",
	})
	if err != nil {
		return Review{}, Usage{}, err
	}
	duration := time.Since(started)

	var response struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return Review{}, Usage{}, fmt.Errorf("model: could not decode the reply: %w", err)
	}

	var text strings.Builder
	for _, block := range response.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	review, err := ParseReview(text.String())
	usage := Usage{
		Model:            c.Model,
		PromptTokens:     response.Usage.InputTokens,
		CompletionTokens: response.Usage.OutputTokens,
		Duration:         duration,
	}
	return review, usage, err
}

func (c *OpenAI) post(ctx context.Context, url string, payload []byte, headers map[string]string) ([]byte, error) {
	return postJSON(ctx, c.HTTP, c.Policy, c.Timeout, url, payload, headers)
}

func (c *Anthropic) post(ctx context.Context, url string, payload []byte, headers map[string]string) ([]byte, error) {
	return postJSON(ctx, c.HTTP, c.Policy, c.Timeout, url, payload, headers)
}

func postJSON(ctx context.Context, client *http.Client, policy httpx.Policy, timeout time.Duration, url string, payload []byte, headers map[string]string) ([]byte, error) {
	if timeout > 0 {
		// A provider that accepts a request and then never answers must not
		// hold a review for ever.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	response, err := policy.Do(ctx, client, func(ctx context.Context) (*http.Request, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("User-Agent", "code-review-agent")
		for name, value := range headers {
			request.Header.Set(name, value)
		}
		return request, nil
	})
	if err != nil {
		return nil, fmt.Errorf("model: %w", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("model: could not read the reply: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("model: the provider answered %d: %s", response.StatusCode, truncate(body, 400))
	}
	return body, nil
}

func truncate(body []byte, limit int) string {
	if len(body) <= limit {
		return string(body)
	}
	return string(body[:limit]) + "..."
}
