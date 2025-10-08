package classify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

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
