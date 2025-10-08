package classify

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// LearningEngine processes user feedback to improve classification
type LearningEngine struct {
	db *sql.DB
}

type Feedback struct {
	ID        int
	MessageID string
	Action    string
	Category  string
	Priority  string
	Reason    string
	CreatedAt time.Time
}

type LearnedRule struct {
	ID            int
	Pattern       string
	PatternType   string // "sender_domain", "sender_email", "subject_keyword", "contextual"
	Category      string
	Priority      string
	Confidence    float64
	Source        string
	FeedbackCount int
	CreatedAt     time.Time
	LastApplied   *time.Time
}

func NewLearningEngine(db *sql.DB) *LearningEngine {
	return &LearningEngine{db: db}
}

// ProcessPendingFeedback analyzes unprocessed feedback and generates rules
// embeddings parameter is optional - if provided, will also create learned examples for semantic categories
func (e *LearningEngine) ProcessPendingFeedback(ctx context.Context, embeddings *EmbeddingsClassifier) (int, error) {
	// Get unprocessed feedback
	feedbacks, err := e.getPendingFeedback(ctx)
	if err != nil {
		return 0, err
	}

	rulesCreated := 0
	for _, fb := range feedbacks {
		// Get the email details to extract patterns
		email, err := e.getEmailDetails(ctx, fb.MessageID)
		if err != nil {
			continue
		}

		// Determine if this is a semantic category (needs embedding) or structural (needs pattern)
		isSemanticCategory := isSemanticCategory(fb.Category)

		if isSemanticCategory && embeddings != nil {
			// Add as learned example for semantic similarity
			if err := embeddings.AddLearnedExample(ctx, email, fb.Category); err == nil {
				rulesCreated++
			}
		} else {
			// Extract patterns for rule-based matching
			patterns := e.extractPatterns(email)

			// Create or update rules
			for _, pattern := range patterns {
				if err := e.createOrUpdateRule(ctx, pattern, fb); err != nil {
					continue
				}
				rulesCreated++
			}
		}

		// Mark feedback as applied
		if err := e.markFeedbackApplied(ctx, fb.ID); err != nil {
			continue
		}
	}

	return rulesCreated, nil
}

// isSemanticCategory returns true for categories that need embedding-based learning
func isSemanticCategory(category string) bool {
	semanticCategories := map[string]bool{
		"People":          true,
		"Clients":         true,
		"Community":       true,
		"Projects":        true,
		"Product Updates": true,
		"Later":           true,
	}
	return semanticCategories[category]
}

// extractPatterns identifies learnable patterns from an email
func (e *LearningEngine) extractPatterns(email EmailDetails) []Pattern {
	patterns := []Pattern{}

	// Sender domain pattern (most reliable)
	if domain := extractDomain(email.From); domain != "" {
		patterns = append(patterns, Pattern{
			Type:       "sender_domain",
			Value:      "from:" + domain,
			Confidence: 0.85,
		})
	}

	// Full sender email (for specific senders)
	if email.From != "" {
		patterns = append(patterns, Pattern{
			Type:       "sender_email",
			Value:      "from:" + email.From,
			Confidence: 0.9,
		})
	}

	// Subject keywords (for specific categories)
	keywords := extractKeywords(email.Subject)
	for _, kw := range keywords {
		patterns = append(patterns, Pattern{
			Type:       "subject_keyword",
			Value:      "subject:" + kw,
			Confidence: 0.7,
		})
	}

	return patterns
}

type Pattern struct {
	Type       string
	Value      string
	Confidence float64
}

type EmailDetails struct {
	MessageID string
	From      string
	Subject   string
	Body      string
}

func (e *LearningEngine) createOrUpdateRule(ctx context.Context, pattern Pattern, feedback Feedback) error {
	// Check if rule already exists
	var existingID int
	var existingCount int
	var existingConfidence float64

	err := e.db.QueryRowContext(ctx, `
		SELECT id, feedback_count, confidence
		FROM learned_rules
		WHERE pattern = ? AND category = ?
	`, pattern.Value, feedback.Category).Scan(&existingID, &existingCount, &existingConfidence)

	if err == sql.ErrNoRows {
		// Create new rule
		confidence := pattern.Confidence
		if feedback.Action == "spam" {
			confidence = 0.95 // High confidence for spam
		}

		_, err := e.db.ExecContext(ctx, `
			INSERT INTO learned_rules
			(pattern, pattern_type, category, priority, confidence, source, feedback_count, created_at)
			VALUES (?, ?, ?, ?, ?, ?, 1, datetime('now'))
		`, pattern.Value, pattern.Type, feedback.Category, feedback.Priority, confidence, "user_feedback")

		return err
	} else if err != nil {
		return err
	}

	// Update existing rule
	newCount := existingCount + 1
	newConfidence := calculateConfidence(existingConfidence, newCount)

	_, err = e.db.ExecContext(ctx, `
		UPDATE learned_rules
		SET feedback_count = ?,
		    confidence = ?,
		    priority = ?
		WHERE id = ?
	`, newCount, newConfidence, feedback.Priority, existingID)

	return err
}

func calculateConfidence(baseConfidence float64, feedbackCount int) float64 {
	// Increase confidence with more feedback
	switch {
	case feedbackCount >= 5:
		return 0.95
	case feedbackCount >= 3:
		return 0.85
	case feedbackCount >= 2:
		return 0.75
	default:
		return baseConfidence
	}
}

func (e *LearningEngine) getPendingFeedback(ctx context.Context) ([]Feedback, error) {
	rows, err := e.db.QueryContext(ctx, `
		SELECT id, message_id, action, category, priority, reason, created_at
		FROM user_feedback
		WHERE applied = 0
		ORDER BY created_at ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var feedbacks []Feedback
	for rows.Next() {
		var fb Feedback
		if err := rows.Scan(&fb.ID, &fb.MessageID, &fb.Action, &fb.Category, &fb.Priority, &fb.Reason, &fb.CreatedAt); err != nil {
			continue
		}
		feedbacks = append(feedbacks, fb)
	}

	return feedbacks, rows.Err()
}

func (e *LearningEngine) getEmailDetails(ctx context.Context, messageID string) (EmailDetails, error) {
	var email EmailDetails
	err := e.db.QueryRowContext(ctx, `
		SELECT id, from_addr, subject
		FROM messages
		WHERE id = ?
	`, messageID).Scan(&email.MessageID, &email.From, &email.Subject)

	return email, err
}

func (e *LearningEngine) markFeedbackApplied(ctx context.Context, feedbackID int) error {
	_, err := e.db.ExecContext(ctx, `
		UPDATE user_feedback
		SET applied = 1
		WHERE id = ?
	`, feedbackID)
	return err
}

// GetLearnedRules retrieves all learned rules
func (e *LearningEngine) GetLearnedRules(ctx context.Context) ([]LearnedRule, error) {
	rows, err := e.db.QueryContext(ctx, `
		SELECT id, pattern, pattern_type, category, priority, confidence, source, feedback_count, created_at, last_applied
		FROM learned_rules
		ORDER BY confidence DESC, feedback_count DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rules []LearnedRule
	for rows.Next() {
		var rule LearnedRule
		if err := rows.Scan(&rule.ID, &rule.Pattern, &rule.PatternType, &rule.Category, &rule.Priority,
			&rule.Confidence, &rule.Source, &rule.FeedbackCount, &rule.CreatedAt, &rule.LastApplied); err != nil {
			continue
		}
		rules = append(rules, rule)
	}

	return rules, rows.Err()
}

// ApplyLearnedRules checks if an email matches any learned rules
func (e *LearningEngine) ApplyLearnedRules(ctx context.Context, email EmailInput) (*Result, bool) {
	rules, err := e.GetLearnedRules(ctx)
	if err != nil {
		return nil, false
	}

	// Try to match rules by priority order
	for _, rule := range rules {
		if e.matchesRule(email, rule) {
			// Update last_applied timestamp
			e.db.ExecContext(ctx, `
				UPDATE learned_rules
				SET last_applied = datetime('now')
				WHERE id = ?
			`, rule.ID)

			return &Result{
				Category:   rule.Category,
				Confidence: rule.Confidence,
				Method:     "learned_rule",
				Reasoning:  fmt.Sprintf("Matched learned pattern: %s (from %d corrections)", rule.Pattern, rule.FeedbackCount),
			}, true
		}
	}

	return nil, false
}

func (e *LearningEngine) matchesRule(email EmailInput, rule LearnedRule) bool {
	switch rule.PatternType {
	case "sender_domain":
		domain := extractDomain(email.From)
		return rule.Pattern == "from:"+domain

	case "sender_email":
		return rule.Pattern == "from:"+email.From

	case "subject_keyword":
		keyword := strings.TrimPrefix(rule.Pattern, "subject:")
		return strings.Contains(strings.ToLower(email.Subject), strings.ToLower(keyword))

	default:
		return false
	}
}

// Helper functions

func extractDomain(email string) string {
	parts := strings.Split(email, "@")
	if len(parts) != 2 {
		return ""
	}
	return parts[1]
}

func extractKeywords(subject string) []string {
	// Extract significant keywords from subject
	// This is simplified - could be more sophisticated
	lower := strings.ToLower(subject)
	keywords := []string{}

	significantWords := []string{
		"invoice", "receipt", "payment", "bill", "subscription",
		"alert", "urgent", "critical", "warning", "error",
		"verification", "confirm", "code", "security",
		"update", "release", "announcement",
	}

	for _, word := range significantWords {
		if strings.Contains(lower, word) {
			keywords = append(keywords, word)
		}
	}

	return keywords
}

// ExtractMetadata extracts useful metadata from email content
func ExtractMetadata(subject, body string) map[string]string {
	metadata := make(map[string]string)

	text := subject + " " + body

	// Extract dollar amounts
	amountRegex := regexp.MustCompile(`\$[\d,]+\.?\d*`)
	if matches := amountRegex.FindAllString(text, -1); len(matches) > 0 {
		metadata["amount"] = matches[0]
	}

	// Extract percentages
	percentRegex := regexp.MustCompile(`(\d+)%`)
	if matches := percentRegex.FindStringSubmatch(text); len(matches) > 1 {
		metadata["percentage"] = matches[1] + "%"
	}

	// Extract account numbers (simple pattern)
	accountRegex := regexp.MustCompile(`account[:\s]+(\d{4,})`)
	if matches := accountRegex.FindStringSubmatch(strings.ToLower(text)); len(matches) > 1 {
		metadata["account"] = matches[1]
	}

	// Extract change indicators
	changeRegex := regexp.MustCompile(`(increase|decrease)d?\s+(?:by\s+)?(\d+%|\$[\d,]+)`)
	if matches := changeRegex.FindStringSubmatch(strings.ToLower(text)); len(matches) > 2 {
		metadata["change_type"] = matches[1]
		metadata["change_amount"] = matches[2]
	}

	return metadata
}
