package summarize

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	"github.com/clauswitt/automatr/internal/classify"
	"github.com/clauswitt/automatr/internal/graph"
)

type DigestService struct {
	db         *sql.DB
	classifier *classify.Classifier
	client     *graph.Client
}

type EmailSummary struct {
	MessageID   string
	Subject     string
	From        string
	Category    string
	Priority    string
	Summary     string
	WebLink     string
	ReceivedAt  time.Time
}

type DigestSummary struct {
	ID           int
	StartTime    time.Time
	EndTime      time.Time
	MetaSummary  string
	Categories   map[string][]EmailSummary
	CreatedAt    time.Time
}

func NewDigestService(db *sql.DB, classifier *classify.Classifier, client *graph.Client) *DigestService {
	return &DigestService{
		db:         db,
		classifier: classifier,
		client:     client,
	}
}

// GetOrCreateSummary gets a cached summary or generates a new one
func (d *DigestService) GetOrCreateSummary(ctx context.Context, messageID string, email classify.EmailInput, category, webLink string) (string, error) {
	// Check cache first
	var summary string
	err := d.db.QueryRowContext(ctx, `SELECT summary FROM email_summaries WHERE message_id = ?`, messageID).Scan(&summary)
	if err == nil {
		return summary, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}

	// Generate new summary (uses webLink for Outlook)
	summary, err = d.classifier.SummarizeEmail(ctx, email, category, webLink)
	if err != nil {
		return "", err
	}

	// Cache it
	_, err = d.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO email_summaries (message_id, summary, created_at)
		VALUES (?, ?, datetime('now'))
	`, messageID, summary)

	return summary, err
}

// InvalidateSummary removes a cached summary (for when rules change)
func (d *DigestService) InvalidateSummary(ctx context.Context, messageID string) error {
	_, err := d.db.ExecContext(ctx, `DELETE FROM email_summaries WHERE message_id = ?`, messageID)
	return err
}

// InvalidateAllSummaries clears all cached summaries
func (d *DigestService) InvalidateAllSummaries(ctx context.Context) error {
	_, err := d.db.ExecContext(ctx, `DELETE FROM email_summaries`)
	return err
}

// GetLastDigestEndTime returns the end time of the last digest, or nil if none exists
func (d *DigestService) GetLastDigestEndTime(ctx context.Context) (*time.Time, error) {
	var endTime time.Time
	err := d.db.QueryRowContext(ctx, `
		SELECT end_time FROM digest_summaries
		ORDER BY end_time DESC
		LIMIT 1
	`).Scan(&endTime)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	return &endTime, nil
}

// CreateDigest generates a digest summary for a time period
func (d *DigestService) CreateDigest(ctx context.Context, startTime, endTime time.Time) (*DigestSummary, error) {
	// Get all messages in the time range (no deduplication - show all messages in range)
	// Use datetime() to handle timezone-aware timestamps correctly
	rows, err := d.db.QueryContext(ctx, `
		SELECT m.id, m.subject, m.from_addr, m.category, m.received_at, m.raw_link, m.internet_message_id
		FROM messages m
		WHERE datetime(m.received_at) >= datetime(?) AND datetime(m.received_at) < datetime(?)
		ORDER BY m.received_at DESC
	`, startTime.Format(time.RFC3339), endTime.Format(time.RFC3339))

	if err != nil {
		return nil, err
	}

	// Collect all message info first (don't hold cursor while processing)
	type msgInfo struct {
		id                 string
		subject            string
		fromAddr           string
		category           string
		receivedAt         time.Time
		webLink            string
		internetMessageId  sql.NullString
	}

	var messagesToProcess []msgInfo
	for rows.Next() {
		var msg msgInfo
		if err := rows.Scan(&msg.id, &msg.subject, &msg.fromAddr, &msg.category, &msg.receivedAt, &msg.webLink, &msg.internetMessageId); err != nil {
			continue
		}
		messagesToProcess = append(messagesToProcess, msg)
	}
	rows.Close() // Close cursor BEFORE processing

	categorizedEmails := make(map[string][]EmailSummary)

	// Now process all messages without holding cursor
	for _, msgInfo := range messagesToProcess {
		// Check if we already have a cached summary
		var cachedSummary string
		err := d.db.QueryRowContext(ctx, `SELECT summary FROM email_summaries WHERE message_id = ?`, msgInfo.id).Scan(&cachedSummary)

		// If we have a cached summary, use it with the stored webLink (no API fetch needed)
		if err == nil && cachedSummary != "" {
			categorizedEmails[msgInfo.category] = append(categorizedEmails[msgInfo.category], EmailSummary{
				MessageID:  msgInfo.id,
				Subject:    msgInfo.subject,
				From:       msgInfo.fromAddr,
				Category:   msgInfo.category,
				Summary:    cachedSummary,
				WebLink:    msgInfo.webLink, // Use stored link (already in final location from auto-archive)
				ReceivedAt: msgInfo.receivedAt,
			})
			continue
		}

		// No cached summary - need to fetch message body for summarization
		var msg *graph.Message
		if msgInfo.internetMessageId.Valid && msgInfo.internetMessageId.String != "" {
			msg, _ = d.client.GetMessageByInternetMessageId(ctx, msgInfo.internetMessageId.String)
		}
		if msg == nil {
			msg, _ = d.client.GetMessage(ctx, msgInfo.id)
		}

		// If we couldn't fetch the message, use simple summary
		if msg == nil {
			categorizedEmails[msgInfo.category] = append(categorizedEmails[msgInfo.category], EmailSummary{
				MessageID:  msgInfo.id,
				Subject:    msgInfo.subject,
				From:       msgInfo.fromAddr,
				Category:   msgInfo.category,
				Summary:    fmt.Sprintf("**%s** - %s", msgInfo.subject, msgInfo.fromAddr),
				WebLink:    msgInfo.webLink,
				ReceivedAt: msgInfo.receivedAt,
			})
			continue
		}

		// Generate new summary
		bodyText := msg.Body.Content
		if len(bodyText) > 1000 {
			bodyText = bodyText[:1000]
		}

		emailInput := classify.EmailInput{
			From:    msg.From.EmailAddress.Address,
			Subject: msg.Subject,
			Body:    bodyText,
		}

		summary, err := d.GetOrCreateSummary(ctx, msgInfo.id, emailInput, msgInfo.category, msgInfo.webLink)
		if err != nil {
			summary = fmt.Sprintf("**%s** - %s", msgInfo.subject, msgInfo.fromAddr)
		}

		categorizedEmails[msgInfo.category] = append(categorizedEmails[msgInfo.category], EmailSummary{
			MessageID:  msgInfo.id,
			Subject:    msgInfo.subject,
			From:       msgInfo.fromAddr,
			Category:   msgInfo.category,
			Summary:    summary,
			WebLink:    msgInfo.webLink, // Use stored link
			ReceivedAt: msgInfo.receivedAt,
		})
	}

	// Create digest record
	result, err := d.db.ExecContext(ctx, `
		INSERT INTO digest_summaries (start_time, end_time, created_at)
		VALUES (?, ?, datetime('now'))
	`, startTime, endTime)

	if err != nil {
		return nil, err
	}

	id, _ := result.LastInsertId()

	// Store webLinks for this digest (so they're preserved even after archiving)
	for _, emails := range categorizedEmails {
		for _, email := range emails {
			_, _ = d.db.ExecContext(ctx, `
				INSERT INTO digest_emails (digest_id, message_id, weblink)
				VALUES (?, ?, ?)
			`, id, email.MessageID, email.WebLink)
		}
	}

	digest := &DigestSummary{
		ID:         int(id),
		StartTime:  startTime,
		EndTime:    endTime,
		Categories: categorizedEmails,
		CreatedAt:  time.Now(),
	}

	return digest, nil
}

// GetCategoryImportance returns a sorted list of categories by importance
func (d *DigestService) GetCategoryImportance(ctx context.Context) ([]string, error) {
	// Priority mapping for default priorities
	priorityOrder := map[string]int{
		"urgent": 1,
		"high":   2,
		"medium": 3,
		"low":    4,
		"spam":   5,
	}

	rows, err := d.db.QueryContext(ctx, `
		SELECT name, priority_default FROM categories ORDER BY name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type categoryPriority struct {
		name     string
		priority int
	}

	var categories []categoryPriority
	for rows.Next() {
		var name, priorityDefault string
		if err := rows.Scan(&name, &priorityDefault); err != nil {
			continue
		}

		priority, ok := priorityOrder[priorityDefault]
		if !ok {
			priority = 3 // default to medium
		}

		categories = append(categories, categoryPriority{name: name, priority: priority})
	}

	// Sort by priority
	sort.Slice(categories, func(i, j int) bool {
		return categories[i].priority < categories[j].priority
	})

	// Extract names
	names := make([]string, len(categories))
	for i, cat := range categories {
		names[i] = cat.name
	}

	return names, nil
}

// GenerateMetaSummary creates a brief overview of the digest
func (d *DigestService) GenerateMetaSummary(ctx context.Context, digest *DigestSummary) (string, error) {
	totalEmails := 0
	urgentCount := 0

	categoryBreakdown := make(map[string]int)

	for category, emails := range digest.Categories {
		count := len(emails)
		totalEmails += count
		categoryBreakdown[category] = count

		// Count as urgent if it's in a high-priority category
		cat, err := d.classifier.GetCategoryManager().GetCategory(ctx, category)
		if err == nil && cat != nil {
			if cat.PriorityDefault == "urgent" || cat.PriorityDefault == "high" {
				urgentCount += count
			}
		}
	}

	if totalEmails == 0 {
		return "No emails in this period.", nil
	}

	metaSummary := fmt.Sprintf("📧 **%d emails** received", totalEmails)

	if urgentCount > 0 {
		metaSummary += fmt.Sprintf(" (%d urgent/high priority)", urgentCount)
	}

	metaSummary += "\n\n**Breakdown by category:**\n"

	// Get sorted categories
	sortedCategories, _ := d.GetCategoryImportance(ctx)

	for _, category := range sortedCategories {
		if count, ok := categoryBreakdown[category]; ok && count > 0 {
			metaSummary += fmt.Sprintf("- %s: %d\n", category, count)
		}
	}

	// Store meta summary
	_, err := d.db.ExecContext(ctx, `
		UPDATE digest_summaries SET meta_summary = ? WHERE id = ?
	`, metaSummary, digest.ID)

	if err != nil {
		return metaSummary, err
	}

	digest.MetaSummary = metaSummary
	return metaSummary, nil
}

// ArchiveDigestEmails archives emails from the digest where keep_in_inbox = 0
// Note: This will invalidate webLinks, so only use after digest is viewed/exported
func (d *DigestService) ArchiveDigestEmails(ctx context.Context, digest *DigestSummary) (int, error) {
	// Get Archive folder ID
	archiveFolderID, err := d.client.GetFolder(ctx, "Archive")
	if err != nil {
		return 0, fmt.Errorf("failed to get Archive folder: %w", err)
	}

	archivedCount := 0

	// Process each category
	for category, emails := range digest.Categories {
		// Check if this category should be archived
		cat, err := d.classifier.GetCategoryManager().GetCategory(ctx, category)
		if err != nil || cat == nil {
			continue // Skip if category not found
		}

		// Only archive if keep_in_inbox is false
		if cat.KeepInInbox {
			continue
		}

		// Archive all emails in this category
		for _, email := range emails {
			movedMsg, err := d.client.MoveMessage(ctx, email.MessageID, archiveFolderID)
			if err != nil {
				// Skip 404 errors (already archived) silently
				// Only log other errors
				if err.Error() != "" && !contains(err.Error(), "404") && !contains(err.Error(), "ErrorItemNotFound") {
					fmt.Printf("Failed to archive message %s: %v\n", email.MessageID, err)
				}
				continue
			}

			// Update database with new ID and webLink from archived location
			if movedMsg != nil {
				// Update both ID and webLink since ID changes when moved to Archive
				if movedMsg.WebLink != "" {
					_, _ = d.db.ExecContext(ctx, `UPDATE messages SET id = ?, raw_link = ? WHERE id = ?`,
						movedMsg.ID, movedMsg.WebLink, email.MessageID)
				}
			}

			archivedCount++
		}
	}

	return archivedCount, nil
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) && (s[:len(substr)] == substr || s[len(s)-len(substr):] == substr || hasSubstring(s, substr)))
}

func hasSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
