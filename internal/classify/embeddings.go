package classify

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
)

// EmbeddingsClassifier uses semantic similarity for classification
type EmbeddingsClassifier struct {
	provider       LLMProvider
	db             *sql.DB
	examples       map[string][]float64 // Category -> example embedding (hardcoded)
	learnedExamples map[string][]LearnedExample // Category -> list of learned examples
}

type LearnedExample struct {
	Text      string
	Embedding []float64
	CreatedAt string
}

func NewEmbeddingsClassifier(provider LLMProvider, db *sql.DB) *EmbeddingsClassifier {
	return &EmbeddingsClassifier{
		provider:        provider,
		db:              db,
		examples:        buildCategoryExamples(),
		learnedExamples: make(map[string][]LearnedExample),
	}
}

// Initialize generates and caches embeddings for category examples
func (c *EmbeddingsClassifier) Initialize(ctx context.Context) error {
	// Check if we already have cached category embeddings
	for category, embedding := range c.examples {
		if len(embedding) == 0 {
			// Reuse the cached embedding from an earlier run
			if cached, err := c.getCachedEmbedding(ctx, "category:"+category); err == nil && len(cached) > 0 {
				c.examples[category] = cached
				continue
			}

			// Generate embedding for this category example
			text := getCategoryExampleText(category)
			emb, err := c.provider.GenerateEmbedding(ctx, text)
			if err != nil {
				return fmt.Errorf("failed to generate embedding for %s: %w", category, err)
			}
			c.examples[category] = emb

			// Cache it
			if err := c.cacheEmbedding(ctx, "category:"+category, emb); err != nil {
				return err
			}
		}
	}

	// Load learned examples from database
	if err := c.loadLearnedExamples(ctx); err != nil {
		return fmt.Errorf("failed to load learned examples: %w", err)
	}

	return nil
}

// Classify using semantic similarity
func (c *EmbeddingsClassifier) Classify(ctx context.Context, email EmailInput) (*Classification, bool) {
	// Create text representation of email
	text := fmt.Sprintf("From: %s\nSubject: %s\n%s",
		email.From,
		email.Subject,
		truncate(email.Body, 500))

	// Try to get cached embedding
	embedding, err := c.getCachedEmbedding(ctx, text)
	if err != nil || embedding == nil {
		// Generate new embedding
		embedding, err = c.provider.GenerateEmbedding(ctx, text)
		if err != nil {
			return nil, false
		}

		// Cache it
		_ = c.cacheEmbedding(ctx, text, embedding)
	}

	// Find best matching category
	bestCategory := ""
	bestSimilarity := 0.0
	isLearned := false

	// First check learned examples (higher priority)
	for category, examples := range c.learnedExamples {
		for _, example := range examples {
			if len(example.Embedding) == 0 {
				continue
			}
			similarity := cosineSimilarity(embedding, example.Embedding)
			// Boost learned examples slightly
			boostedSimilarity := similarity * 1.1
			if boostedSimilarity > bestSimilarity {
				bestSimilarity = boostedSimilarity
				bestCategory = category
				isLearned = true
			}
		}
	}

	// Then check hardcoded examples
	for category, categoryEmb := range c.examples {
		if len(categoryEmb) == 0 {
			continue
		}
		similarity := cosineSimilarity(embedding, categoryEmb)
		if similarity > bestSimilarity {
			bestSimilarity = similarity
			bestCategory = category
			isLearned = false
		}
	}

	// Threshold for confidence (slightly lower for learned examples)
	threshold := 0.7
	if isLearned {
		threshold = 0.65 // More lenient for user-corrected examples
	}

	if bestSimilarity < threshold {
		return nil, false
	}

	reasoning := fmt.Sprintf("Semantic similarity: %.2f", bestSimilarity)
	if isLearned {
		reasoning = fmt.Sprintf("Learned example match: %.2f", bestSimilarity)
	}

	return &Classification{
		Category:   bestCategory,
		Confidence: bestSimilarity,
		Reasoning:  reasoning,
	}, true
}

func (c *EmbeddingsClassifier) cacheEmbedding(ctx context.Context, text string, embedding []float64) error {
	embJSON, err := json.Marshal(embedding)
	if err != nil {
		return err
	}

	_, err = c.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO embeddings (text_hash, embedding, created_at)
		VALUES (?, ?, datetime('now'))
	`, hashText(text), string(embJSON))

	return err
}

func (c *EmbeddingsClassifier) getCachedEmbedding(ctx context.Context, text string) ([]float64, error) {
	var embJSON string
	err := c.db.QueryRowContext(ctx, `
		SELECT embedding FROM embeddings WHERE text_hash = ?
	`, hashText(text)).Scan(&embJSON)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var embedding []float64
	if err := json.Unmarshal([]byte(embJSON), &embedding); err != nil {
		return nil, err
	}

	return embedding, nil
}

// cosineSimilarity calculates cosine similarity between two vectors
func cosineSimilarity(a, b []float64) float64 {
	if len(a) != len(b) {
		return 0
	}

	var dotProduct, normA, normB float64
	for i := 0; i < len(a); i++ {
		dotProduct += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}

	if normA == 0 || normB == 0 {
		return 0
	}

	return dotProduct / (math.Sqrt(normA) * math.Sqrt(normB))
}

func hashText(text string) string {
	// Simple hash for caching (could use crypto/sha256 for production)
	if len(text) > 100 {
		return text[:100]
	}
	return text
}

// loadLearnedExamples loads user-corrected examples from database
func (c *EmbeddingsClassifier) loadLearnedExamples(ctx context.Context) error {
	rows, err := c.db.QueryContext(ctx, `
		SELECT category, text, embedding, created_at
		FROM learned_examples
		ORDER BY created_at DESC
		LIMIT 100
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	c.learnedExamples = make(map[string][]LearnedExample)

	for rows.Next() {
		var category, text, embJSON, createdAt string
		if err := rows.Scan(&category, &text, &embJSON, &createdAt); err != nil {
			continue
		}

		var embedding []float64
		if err := json.Unmarshal([]byte(embJSON), &embedding); err != nil {
			continue
		}

		c.learnedExamples[category] = append(c.learnedExamples[category], LearnedExample{
			Text:      text,
			Embedding: embedding,
			CreatedAt: createdAt,
		})
	}

	return rows.Err()
}

// AddLearnedExample adds a user-corrected email as a learned example
func (c *EmbeddingsClassifier) AddLearnedExample(ctx context.Context, email EmailDetails, category string) error {
	// Create text representation
	text := fmt.Sprintf("From: %s\nSubject: %s\n%s",
		email.From,
		email.Subject,
		truncate(email.Body, 500))

	// Generate embedding
	embedding, err := c.provider.GenerateEmbedding(ctx, text)
	if err != nil {
		return fmt.Errorf("failed to generate embedding: %w", err)
	}

	// Store in database
	embJSON, err := json.Marshal(embedding)
	if err != nil {
		return err
	}

	_, err = c.db.ExecContext(ctx, `
		INSERT INTO learned_examples (category, text, embedding, created_at)
		VALUES (?, ?, ?, datetime('now'))
	`, category, text, string(embJSON))

	if err != nil {
		return err
	}

	// Add to in-memory cache
	c.learnedExamples[category] = append(c.learnedExamples[category], LearnedExample{
		Text:      text,
		Embedding: embedding,
		CreatedAt: "",
	})

	return nil
}

// buildCategoryExamples creates empty placeholders for category embeddings
func buildCategoryExamples() map[string][]float64 {
	return map[string][]float64{
		"People":          nil,
		"Clients":         nil,
		"Finance":         nil,
		"Auth":            nil,
		"Marketing":       nil,
		"Product Updates": nil,
		"Ops":             nil,
		"Community":       nil,
		"Projects":        nil,
		"Later":           nil,
	}
}

// getCategoryExampleText returns representative text for each category
func getCategoryExampleText(category string) string {
	examples := map[string]string{
		"People":          "Hey, can we catch up tomorrow? Let me know when works for you. Looking forward to our discussion about the project.",
		"Clients":         "Hi, I'm interested in your services and would like to schedule a demo. Could we set up a call this week to discuss pricing and implementation?",
		"Finance":         "Your monthly invoice for $99.00 is attached. Payment is due by the end of the month. Thank you for your business.",
		"Auth":            "Your verification code is 123456. Use this code to verify your email address. This code expires in 10 minutes.",
		"Marketing":       "This week only: 50% off all items! Don't miss out on our biggest sale of the year. Shop now and save big. Limited time offer!",
		"Product Updates": "Version 2.0 has been released with exciting new features including dark mode, improved performance, and better search capabilities.",
		"Ops":             "Alert: Server CPU usage has exceeded 90% threshold for the past 15 minutes. Please investigate immediately. System: prod-server-01",
		"Community":       "New reply to your post on the forum. John Doe commented on your question about database optimization. Click here to view the discussion.",
		"Projects":        "Project status update: The feature implementation is on track for next week's sprint. All tests are passing and code review is complete.",
		"Later":           "Miscellaneous notification that doesn't require immediate attention. This is a low priority update for your information.",
	}
	return examples[category]
}
