package classify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrOllamaUnavailable is returned when the Ollama server is unreachable.
// Callers can use errors.Is to detect this and fall back to a secondary provider.
var ErrOllamaUnavailable = errors.New("ollama unavailable")

// LLMProvider abstracts LLM API calls for classification
type LLMProvider interface {
	Classify(ctx context.Context, email EmailContent, allowedCategories []string) (*Classification, error)
	GenerateEmbedding(ctx context.Context, text string) ([]float64, error)
	Summarize(ctx context.Context, email EmailContent, instructions string) (string, error)
}

type EmailContent struct {
	From    string
	Subject string
	Body    string // First ~1000 chars
}

type Classification struct {
	Category   string  `json:"category"`
	Confidence float64 `json:"confidence"`
	Reasoning  string  `json:"reasoning"`
}

// OpenAIProvider implements LLMProvider for OpenAI
type OpenAIProvider struct {
	apiKey string
	model  string
	client *http.Client
}

func NewOpenAIProvider(apiKey, model string) *OpenAIProvider {
	return &OpenAIProvider{
		apiKey: apiKey,
		model:  model,
		client: &http.Client{Timeout: 60 * time.Second}, // Increased timeout for summarization
	}
}

func (p *OpenAIProvider) Classify(ctx context.Context, email EmailContent, allowedCategories []string) (*Classification, error) {
	prompt := buildClassificationPrompt(email, allowedCategories)

	reqBody := map[string]interface{}{
		"model": p.model,
		"messages": []map[string]string{
			{"role": "system", "content": "You are an email classification assistant. Return only valid JSON."},
			{"role": "user", "content": prompt},
		},
		"temperature": 0.3,
		"response_format": map[string]string{
			"type": "json_object",
		},
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.openai.com/v1/chat/completions", bytes.NewReader(jsonData))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("OpenAI API error %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	if len(result.Choices) == 0 {
		return nil, fmt.Errorf("no response from OpenAI")
	}

	var classification Classification
	if err := json.Unmarshal([]byte(result.Choices[0].Message.Content), &classification); err != nil {
		return nil, fmt.Errorf("failed to parse classification: %w", err)
	}

	return &classification, nil
}

func (p *OpenAIProvider) Summarize(ctx context.Context, email EmailContent, instructions string) (string, error) {
	prompt := buildSummarizationPrompt(email, instructions)

	// Use gpt-4o-mini for faster, cheaper summaries (gpt-4o was too slow)
	reqBody := map[string]interface{}{
		"model": "gpt-4o-mini",
		"messages": []map[string]string{
			{"role": "system", "content": "You are an email summarization assistant. Provide concise, actionable summaries."},
			{"role": "user", "content": prompt},
		},
		"temperature": 0.3,
		"max_tokens": 100, // Shorter summaries = faster
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.openai.com/v1/chat/completions", bytes.NewReader(jsonData))
	if err != nil {
		return "", err
	}

	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("OpenAI API error %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	if len(result.Choices) == 0 {
		return "", fmt.Errorf("no response from OpenAI")
	}

	return result.Choices[0].Message.Content, nil
}

func (p *OpenAIProvider) GenerateEmbedding(ctx context.Context, text string) ([]float64, error) {
	reqBody := map[string]interface{}{
		"model": "text-embedding-3-small",
		"input": text,
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.openai.com/v1/embeddings", bytes.NewReader(jsonData))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("OpenAI API error %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	if len(result.Data) == 0 {
		return nil, fmt.Errorf("no embedding returned")
	}

	return result.Data[0].Embedding, nil
}

// AnthropicProvider implements LLMProvider for Anthropic Claude
type AnthropicProvider struct {
	apiKey string
	model  string
	client *http.Client
}

func NewAnthropicProvider(apiKey, model string) *AnthropicProvider {
	return &AnthropicProvider{
		apiKey: apiKey,
		model:  model,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (p *AnthropicProvider) Classify(ctx context.Context, email EmailContent, allowedCategories []string) (*Classification, error) {
	prompt := buildClassificationPrompt(email, allowedCategories)

	reqBody := map[string]interface{}{
		"model": p.model,
		"max_tokens": 300,
		"temperature": 0.3,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(jsonData))
	if err != nil {
		return nil, err
	}

	req.Header.Set("x-api-key", p.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Anthropic API error %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	if len(result.Content) == 0 {
		return nil, fmt.Errorf("no response from Anthropic")
	}

	// Extract JSON from response (Claude might wrap it in markdown)
	text := result.Content[0].Text

	// Try to find JSON in the response
	var classification Classification
	if err := json.Unmarshal([]byte(text), &classification); err != nil {
		// Try to extract JSON from markdown code block
		start := bytes.Index([]byte(text), []byte("{"))
		end := bytes.LastIndex([]byte(text), []byte("}"))
		if start >= 0 && end > start {
			jsonStr := text[start : end+1]
			if err := json.Unmarshal([]byte(jsonStr), &classification); err != nil {
				return nil, fmt.Errorf("failed to parse classification: %w", err)
			}
		} else {
			return nil, fmt.Errorf("failed to parse classification: %w", err)
		}
	}

	return &classification, nil
}

func (p *AnthropicProvider) Summarize(ctx context.Context, email EmailContent, instructions string) (string, error) {
	prompt := buildSummarizationPrompt(email, instructions)

	// Use claude-3-5-sonnet for better summaries
	reqBody := map[string]interface{}{
		"model": "claude-3-5-sonnet-20241022",
		"max_tokens": 150,
		"temperature": 0.3,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(jsonData))
	if err != nil {
		return "", err
	}

	req.Header.Set("x-api-key", p.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("Anthropic API error %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	if len(result.Content) == 0 {
		return "", fmt.Errorf("no response from Anthropic")
	}

	return result.Content[0].Text, nil
}

func (p *AnthropicProvider) GenerateEmbedding(ctx context.Context, text string) ([]float64, error) {
	return nil, fmt.Errorf("Anthropic does not support embeddings - use OpenAI for embeddings")
}

// OllamaProvider implements LLMProvider for a local Ollama server.
// Chat completions use the OpenAI-compatible endpoint (/v1/chat/completions).
// Embeddings use Ollama's native /api/embeddings endpoint.
type OllamaProvider struct {
	baseURL    string
	model      string
	embedModel string
	client     *http.Client
}

func NewOllamaProvider(baseURL, model, embedModel string) *OllamaProvider {
	return &OllamaProvider{
		baseURL:    strings.TrimRight(baseURL, "/"),
		model:      model,
		embedModel: embedModel,
		client:     &http.Client{Timeout: 120 * time.Second},
	}
}

// isConnectionError reports whether err looks like a network-level failure
// talking to Ollama (connection refused, DNS, timeout, etc.), as opposed to
// a successful HTTP exchange that returned an error status.
func isConnectionError(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}
	return false
}

func (p *OllamaProvider) wrapConnErr(err error) error {
	if isConnectionError(err) {
		return fmt.Errorf("%w: %v", ErrOllamaUnavailable, err)
	}
	return err
}

func (p *OllamaProvider) Classify(ctx context.Context, email EmailContent, allowedCategories []string) (*Classification, error) {
	prompt := buildClassificationPrompt(email, allowedCategories)

	reqBody := map[string]interface{}{
		"model": p.model,
		"messages": []map[string]string{
			{"role": "system", "content": "You are an email classification assistant. Return only valid JSON."},
			{"role": "user", "content": prompt},
		},
		"temperature": 0.3,
		"response_format": map[string]string{
			"type": "json_object",
		},
	}

	content, err := p.chatCompletion(ctx, reqBody)
	if err != nil {
		return nil, err
	}

	var classification Classification
	if err := json.Unmarshal([]byte(content), &classification); err != nil {
		// Some local models ignore response_format and wrap JSON in prose/markdown.
		start := strings.Index(content, "{")
		end := strings.LastIndex(content, "}")
		if start >= 0 && end > start {
			if err := json.Unmarshal([]byte(content[start:end+1]), &classification); err != nil {
				return nil, fmt.Errorf("failed to parse classification: %w", err)
			}
		} else {
			return nil, fmt.Errorf("failed to parse classification: %w", err)
		}
	}

	return &classification, nil
}

func (p *OllamaProvider) Summarize(ctx context.Context, email EmailContent, instructions string) (string, error) {
	prompt := buildSummarizationPrompt(email, instructions)

	reqBody := map[string]interface{}{
		"model": p.model,
		"messages": []map[string]string{
			{"role": "system", "content": "You are an email summarization assistant. Provide concise, actionable summaries."},
			{"role": "user", "content": prompt},
		},
		"temperature": 0.3,
		"max_tokens":  150,
	}

	return p.chatCompletion(ctx, reqBody)
}

func (p *OllamaProvider) chatCompletion(ctx context.Context, reqBody map[string]interface{}) (string, error) {
	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	endpoint := p.baseURL + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(jsonData))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", p.wrapConnErr(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		// 5xx from the Ollama process itself is effectively "unavailable"
		if resp.StatusCode >= 500 {
			return "", fmt.Errorf("%w: Ollama API error %d: %s", ErrOllamaUnavailable, resp.StatusCode, string(body))
		}
		return "", fmt.Errorf("Ollama API error %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if len(result.Choices) == 0 {
		return "", fmt.Errorf("no response from Ollama")
	}
	return result.Choices[0].Message.Content, nil
}

func (p *OllamaProvider) GenerateEmbedding(ctx context.Context, text string) ([]float64, error) {
	reqBody := map[string]interface{}{
		"model":  p.embedModel,
		"prompt": text,
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	endpoint := p.baseURL + "/api/embeddings"
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(jsonData))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, p.wrapConnErr(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 500 {
			return nil, fmt.Errorf("%w: Ollama embeddings error %d: %s", ErrOllamaUnavailable, resp.StatusCode, string(body))
		}
		return nil, fmt.Errorf("Ollama embeddings error %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Embedding []float64 `json:"embedding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if len(result.Embedding) == 0 {
		return nil, fmt.Errorf("no embedding returned from Ollama")
	}
	return result.Embedding, nil
}

// FallbackProvider tries the primary provider first and falls back to the
// secondary provider on connection errors or ErrOllamaUnavailable.
type FallbackProvider struct {
	primary   LLMProvider
	secondary LLMProvider
}

func NewFallbackProvider(primary, secondary LLMProvider) *FallbackProvider {
	return &FallbackProvider{primary: primary, secondary: secondary}
}

// shouldFallback decides whether an error from the primary provider is
// recoverable by trying the secondary. Only network-level failures and the
// typed ErrOllamaUnavailable qualify — real API errors (bad key, bad model,
// 4xx payload problems) surface directly.
func shouldFallback(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrOllamaUnavailable) {
		return true
	}
	return isConnectionError(err)
}

func (p *FallbackProvider) Classify(ctx context.Context, email EmailContent, allowedCategories []string) (*Classification, error) {
	log.Printf("classify: using primary provider")
	result, err := p.primary.Classify(ctx, email, allowedCategories)
	if err == nil {
		return result, nil
	}
	if !shouldFallback(err) || p.secondary == nil {
		return nil, err
	}
	log.Printf("classify: primary failed (%v), falling back to secondary", err)
	return p.secondary.Classify(ctx, email, allowedCategories)
}

func (p *FallbackProvider) Summarize(ctx context.Context, email EmailContent, instructions string) (string, error) {
	log.Printf("summarize: using primary provider")
	result, err := p.primary.Summarize(ctx, email, instructions)
	if err == nil {
		return result, nil
	}
	if !shouldFallback(err) || p.secondary == nil {
		return "", err
	}
	log.Printf("summarize: primary failed (%v), falling back to secondary", err)
	return p.secondary.Summarize(ctx, email, instructions)
}

func (p *FallbackProvider) GenerateEmbedding(ctx context.Context, text string) ([]float64, error) {
	log.Printf("embedding: using primary provider")
	result, err := p.primary.GenerateEmbedding(ctx, text)
	if err == nil {
		return result, nil
	}
	if !shouldFallback(err) || p.secondary == nil {
		return nil, err
	}
	log.Printf("embedding: primary failed (%v), falling back to secondary", err)
	return p.secondary.GenerateEmbedding(ctx, text)
}

// buildClassificationPrompt creates the classification prompt
func buildClassificationPrompt(email EmailContent, allowedCategories []string) string {
	// Default category descriptions
	categoryDescriptions := map[string]string{
		"People":           "Direct personal communication from individuals",
		"Clients":          "Business opportunities, potential customers, sales inquiries",
		"Finance":          "Invoices, payments, receipts, financial statements",
		"Auth":             "Login codes, 2FA, password resets, security alerts",
		"Marketing":        "Newsletters, promotional emails, marketing campaigns",
		"Product Updates":  "Release notes, product announcements, changelog",
		"Ops":              "System monitoring, server alerts, error notifications",
		"Community":        "Forum notifications, social network updates",
		"Projects":         "Project-specific work communications",
		"Later":            "Low priority items, misc",
	}

	// Build categories list
	categoriesText := "Categories:\n"
	if len(allowedCategories) > 0 {
		// Only show allowed categories
		categoriesText += "IMPORTANT: You MUST choose ONE of these categories. No other categories are allowed:\n"
		for _, cat := range allowedCategories {
			if desc, ok := categoryDescriptions[cat]; ok {
				categoriesText += fmt.Sprintf("- %s: %s\n", cat, desc)
			} else {
				categoriesText += fmt.Sprintf("- %s\n", cat)
			}
		}
	} else {
		// Show all default categories
		for cat, desc := range categoryDescriptions {
			categoriesText += fmt.Sprintf("- %s: %s\n", cat, desc)
		}
	}

	return fmt.Sprintf(`Classify this email into ONE category.

%s
Email:
From: %s
Subject: %s
Body: %s

Return ONLY valid JSON with this exact structure:
{
  "category": "category_name",
  "confidence": 0.85,
  "reasoning": "brief explanation"
}`, categoriesText, email.From, email.Subject, email.Body)
}

// buildSummarizationPrompt creates the summarization prompt
func buildSummarizationPrompt(email EmailContent, instructions string) string {
	if instructions == "" {
		instructions = "Provide a brief, actionable summary of this email."
	}

	return fmt.Sprintf(`%s

Email:
From: %s
Subject: %s
Body: %s

Provide a concise summary (2-3 sentences maximum).`, instructions, email.From, email.Subject, email.Body)
}
