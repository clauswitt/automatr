package classify

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
)

// Classifier orchestrates the three-layer classification system
type Classifier struct {
	rules      *RuleBasedClassifier
	embeddings *EmbeddingsClassifier
	llm        LLMProvider
	jev        *JevClassifier
	jevMinConf float64
	learning   *LearningEngine
	whitelist  *WhitelistManager
	categories *CategoryManager
	db         *sql.DB
	dryRun     bool
}

type Result struct {
	Category   string
	Confidence float64
	Priority   string            // urgent, high, medium, low, spam
	Method     string            // "learned_rule", "rules", "jev", "embeddings", or "llm"
	Reasoning  string
	Metadata   map[string]string // Extracted metadata (amounts, percentages, etc)
	Triage     *Triage           // Archive-decision probabilities; nil when Jev is unavailable
}

// NewClassifier creates a new classifier with all layers including learning
func NewClassifier(llmProvider LLMProvider, db *sql.DB, dryRun bool) *Classifier {
	return &Classifier{
		rules:      NewRuleBasedClassifier(),
		embeddings: NewEmbeddingsClassifier(llmProvider, db),
		llm:        llmProvider,
		learning:   NewLearningEngine(db),
		whitelist:  NewWhitelistManager(db),
		categories: NewCategoryManager(db),
		db:         db,
		dryRun:     dryRun,
	}
}

// SetJev enables Jev as the primary semantic layer. Emails it is less than
// minConfidence sure about go to "Needs Review" instead of the LLM.
func (c *Classifier) SetJev(jev *JevClassifier, minConfidence float64) {
	c.jev = jev
	c.jevMinConf = minConfidence
}

// Initialize prepares the classifier (generates category embeddings)
func (c *Classifier) Initialize(ctx context.Context) error {
	// Seed default categories if empty
	if err := c.categories.SeedDefaultCategories(ctx); err != nil {
		return fmt.Errorf("failed to seed categories: %w", err)
	}
	if err := c.embeddings.Initialize(ctx); err != nil {
		// Embeddings are an optional layer; without them Classify skips
		// straight to Jev / the LLM rather than refusing to run at all.
		log.Printf("embeddings unavailable, continuing without them: %v", err)
	}
	return nil
}

// Classify categorizes an email and, when Jev is configured, attaches the
// triage probabilities used for the archive decision.
func (c *Classifier) Classify(ctx context.Context, email EmailInput) (*Result, error) {
	result, err := c.classifyLayers(ctx, email)
	if err != nil || c.jev == nil || result.Triage != nil {
		return result, err
	}
	triage, err := c.jev.Triage(ctx, email)
	if err != nil {
		log.Printf("jev triage failed, email will stay in inbox: %v", err)
		return result, nil
	}
	result.Triage = triage
	return result, nil
}

// Triage asks Jev only the archive questions, e.g. for messages classified
// before triage existed.
func (c *Classifier) Triage(ctx context.Context, email EmailInput) (*Triage, error) {
	if c.jev == nil {
		return nil, fmt.Errorf("jev not configured")
	}
	return c.jev.Triage(ctx, email)
}

// classifyLayers runs learned rules → rules → Jev → embeddings → LLM.
func (c *Classifier) classifyLayers(ctx context.Context, email EmailInput) (*Result, error) {
	// Extract metadata first
	metadata := ExtractMetadata(email.Subject, email.Body)

	// Layer 0: Learned Rules (highest priority - user corrections)
	if result, matched := c.learning.ApplyLearnedRules(ctx, email); matched {
		result.Priority = inferPriority(result.Category, metadata)
		result.Metadata = metadata
		return result, nil
	}

	// Layer 1: Hardcoded Rules
	if classification, matched := c.rules.Classify(email); matched {
		return &Result{
			Category:   classification.Category,
			Confidence: classification.Confidence,
			Priority:   inferPriority(classification.Category, metadata),
			Method:     "rules",
			Reasoning:  classification.Reasoning,
			Metadata:   metadata,
		}, nil
	}

	// Layer 2: Jev (calibrated choice over the category list)
	if c.jev != nil {
		result, err := c.classifyWithJev(ctx, email, metadata)
		if err == nil {
			return result, nil
		}
		log.Printf("jev classification failed, falling back to embeddings/LLM: %v", err)
	}

	// Layer 3: Embeddings
	classification, matched := c.embeddings.Classify(ctx, email)
	if matched && classification.Confidence >= 0.7 {
		return &Result{
			Category:   classification.Category,
			Confidence: classification.Confidence,
			Priority:   inferPriority(classification.Category, metadata),
			Method:     "embeddings",
			Reasoning:  classification.Reasoning,
			Metadata:   metadata,
		}, nil
	}

	// Layer 4: LLM Adjudicator
	emailContent := EmailContent{
		From:    email.From,
		Subject: email.Subject,
		Body:    truncate(email.Body, 1000),
	}

	// Check if there's a whitelist with category restrictions
	var allowedCategories []string
	whitelist, wlErr := c.whitelist.CheckWhitelist(ctx, email.From)
	if wlErr == nil && whitelist != nil && len(whitelist.AllowedCategories) > 0 {
		// Whitelist restricts categories
		allowedCategories = whitelist.AllowedCategories
	} else {
		// Use all categories from database
		categoryNames, catErr := c.categories.GetCategoryNames(ctx)
		if catErr == nil && len(categoryNames) > 0 {
			allowedCategories = categoryNames
		}
		// If database fetch fails, allowedCategories stays empty and LLM uses hardcoded defaults
	}

	classification, err := c.llm.Classify(ctx, emailContent, allowedCategories)
	if err != nil {
		// LLM failed - use fallback category
		return &Result{
			Category:   "Other",
			Confidence: 0.3,
			Priority:   "medium",
			Method:     "fallback",
			Reasoning:  "Classification failed: " + err.Error(),
			Metadata:   metadata,
		}, nil
	}

	// Generative models sometimes echo the description from the prompt
	// ("Product Updates: Release notes, ..."); keep just the name.
	if name, _, found := strings.Cut(classification.Category, ":"); found {
		classification.Category = strings.TrimSpace(name)
	}

	// Ensure we never return empty category
	if classification.Category == "" {
		classification.Category = "Other"
	}

	if classification.Confidence < 0.6 {
		// Mark as needs review
		return &Result{
			Category:   "Needs Review",
			Confidence: classification.Confidence,
			Priority:   "medium",
			Method:     "llm",
			Reasoning:  "Low confidence: " + classification.Reasoning,
			Metadata:   metadata,
		}, nil
	}

	return &Result{
		Category:   classification.Category,
		Confidence: classification.Confidence,
		Priority:   inferPriority(classification.Category, metadata),
		Method:     "llm",
		Reasoning:  classification.Reasoning,
		Metadata:   metadata,
	}, nil
}

// classifyWithJev asks Jev to choose among the allowed categories.
func (c *Classifier) classifyWithJev(ctx context.Context, email EmailInput, metadata map[string]string) (*Result, error) {
	all, err := c.categories.ListCategories(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load categories: %w", err)
	}

	allowed := map[string]bool{}
	if wl, err := c.whitelist.CheckWhitelist(ctx, email.From); err == nil && wl != nil {
		for _, name := range wl.AllowedCategories {
			allowed[name] = true
		}
	}

	criteria := map[string]string{}
	for _, cat := range all {
		if len(allowed) > 0 && !allowed[cat.Name] {
			continue
		}
		criteria[cat.Name] = cat.Description
	}

	classification, triage, err := c.jev.Classify(ctx, email, criteria)
	if err != nil {
		return nil, err
	}

	if classification.Confidence < c.jevMinConf {
		return &Result{
			Category:   "Needs Review",
			Confidence: classification.Confidence,
			Priority:   "medium",
			Method:     "jev",
			Reasoning:  "Low confidence: " + classification.Reasoning,
			Metadata:   metadata,
			Triage:     triage,
		}, nil
	}

	return &Result{
		Category:   classification.Category,
		Confidence: classification.Confidence,
		Priority:   inferPriority(classification.Category, metadata),
		Method:     "jev",
		Reasoning:  classification.Reasoning,
		Metadata:   metadata,
		Triage:     triage,
	}, nil
}

// InferPriority is the exported variant of inferPriority, intended for
// callers outside the package (e.g. digest summary generation) that need to
// reproduce the same bucketing used during classification.
func InferPriority(category string, metadata map[string]string) string {
	return inferPriority(category, metadata)
}

// inferPriority determines priority based on category and metadata
func inferPriority(category string, metadata map[string]string) string {
	// Context-aware priority inference
	switch category {
	case "Marketing":
		return "spam"

	case "Auth":
		return "high" // Security codes are time-sensitive

	case "Ops":
		// Check for urgency indicators
		if _, hasChange := metadata["change_amount"]; hasChange {
			return "urgent" // Cost anomalies, error spikes
		}
		return "high"

	case "Finance":
		// Check if it's just a receipt or something urgent
		if _, hasAmount := metadata["amount"]; hasAmount {
			if metadata["change_type"] == "increase" {
				return "high" // Cost increase
			}
			return "low" // Regular receipt
		}
		return "medium"

	case "People", "Clients":
		return "high" // Personal communication is important

	case "Product Updates":
		return "medium"

	case "Community":
		return "low"

	case "Projects":
		return "medium"

	default:
		return "medium"
	}
}

// CacheClassification stores the classification result for future reference
func (c *Classifier) CacheClassification(ctx context.Context, messageID string, result *Result) error {
	_, err := c.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO classification_cache
		(message_id, category, confidence, method, reasoning, classified_at)
		VALUES (?, ?, ?, ?, ?, datetime('now'))
	`, messageID, result.Category, result.Confidence, result.Method, result.Reasoning)

	return err
}

// GetCachedClassification retrieves a cached classification
func (c *Classifier) GetCachedClassification(ctx context.Context, messageID string) (*Result, error) {
	var result Result
	err := c.db.QueryRowContext(ctx, `
		SELECT category, confidence, method, reasoning
		FROM classification_cache
		WHERE message_id = ?
	`, messageID).Scan(&result.Category, &result.Confidence, &result.Method, &result.Reasoning)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	return &result, nil
}

// ProcessFeedback processes pending user feedback to improve classification
func (c *Classifier) ProcessFeedback(ctx context.Context) (int, error) {
	return c.learning.ProcessPendingFeedback(ctx, c.embeddings)
}

// GetWhitelistManager returns the whitelist manager
func (c *Classifier) GetWhitelistManager() *WhitelistManager {
	return c.whitelist
}

// GetCategoryManager returns the category manager
func (c *Classifier) GetCategoryManager() *CategoryManager {
	return c.categories
}

// SummarizeEmail generates a summary for an email using category-specific instructions
func (c *Classifier) SummarizeEmail(ctx context.Context, email EmailInput, category string, webLink string) (string, error) {
	// Get category-specific summarization instructions
	cat, err := c.categories.GetCategory(ctx, category)
	if err != nil || cat == nil {
		// Use default if category not found
		cat = &Category{
			SummarizationPrompt: "Provide a brief, actionable summary of this email.",
		}
	}

	emailContent := EmailContent{
		From:    email.From,
		Subject: email.Subject,
		Body:    truncate(email.Body, 1000),
	}

	summary, err := c.llm.Summarize(ctx, emailContent, cat.SummarizationPrompt)
	if err != nil {
		return "", fmt.Errorf("summarization failed: %w", err)
	}

	// Add Outlook web link (will redirect to desktop app if installed)
	if webLink != "" {
		summary += fmt.Sprintf("\n\n🔗 [Open in Outlook](%s)", webLink)
	}

	return summary, nil
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && s[len(s)-len(substr):] == substr
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}
