package summarize

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"
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
	Archived    bool // auto-archived; listed as a one-liner under "Filed away"
}

type DigestSummary struct {
	ID           int
	StartTime    time.Time
	EndTime      time.Time
	MetaSummary  string
	Categories   map[string][]EmailSummary
	CreatedAt    time.Time
}

// Split separates emails still in the inbox (by category) from archived ones.
func (d *DigestSummary) Split() (inbox map[string][]EmailSummary, filed []EmailSummary) {
	inbox = make(map[string][]EmailSummary)
	for category, emails := range d.Categories {
		for _, e := range emails {
			if e.Archived {
				filed = append(filed, e)
			} else {
				inbox[category] = append(inbox[category], e)
			}
		}
	}
	sort.Slice(filed, func(i, j int) bool { return filed[i].ReceivedAt.After(filed[j].ReceivedAt) })
	return inbox, filed
}

// OrderedCategories returns categoryOrder with any categories present in
// emails but missing from it (e.g. "Needs Review") added: "Needs Review"
// first, since it needs a human, and the rest at the end.
func OrderedCategories(categoryOrder []string, emails map[string][]EmailSummary) []string {
	known := make(map[string]bool, len(categoryOrder))
	for _, c := range categoryOrder {
		known[c] = true
	}
	var extra []string
	for c := range emails {
		if !known[c] && c != "Needs Review" {
			extra = append(extra, c)
		}
	}
	sort.Strings(extra)

	var ordered []string
	if _, ok := emails["Needs Review"]; ok && !known["Needs Review"] {
		ordered = append(ordered, "Needs Review")
	}
	ordered = append(ordered, categoryOrder...)
	return append(ordered, extra...)
}

// WriteMarkdownSections writes the per-category sections for inbox mail,
// followed by a one-line-per-email "Filed away" section for archived mail.
func WriteMarkdownSections(b *strings.Builder, digest *DigestSummary, categoryOrder []string) {
	inbox, filed := digest.Split()

	for _, category := range OrderedCategories(categoryOrder, inbox) {
		emails := inbox[category]
		if len(emails) == 0 {
			continue
		}
		b.WriteString(fmt.Sprintf("## %s (%d)\n\n", category, len(emails)))
		for _, email := range emails {
			// Remove the outlook link suffix from summary if present
			summary := strings.Split(email.Summary, "\n\n🔗")[0]
			b.WriteString(fmt.Sprintf("- %s [↗](%s)\n", summary, email.WebLink))
		}
		b.WriteString("\n")
	}

	if len(filed) > 0 {
		b.WriteString(fmt.Sprintf("## Filed away (%d)\n\n", len(filed)))
		for _, email := range filed {
			b.WriteString(fmt.Sprintf("- %s · %s — %s [↗](%s)\n", email.Category, email.From, email.Subject, email.WebLink))
		}
		b.WriteString("\n")
	}
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
		SELECT m.id, m.subject, m.from_addr, m.category, m.received_at, m.raw_link, m.internet_message_id,
		       m.archived_at IS NOT NULL
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
		archived           bool
	}

	var messagesToProcess []msgInfo
	for rows.Next() {
		var msg msgInfo
		if err := rows.Scan(&msg.id, &msg.subject, &msg.fromAddr, &msg.category, &msg.receivedAt, &msg.webLink, &msg.internetMessageId, &msg.archived); err != nil {
			continue
		}
		messagesToProcess = append(messagesToProcess, msg)
	}
	rows.Close() // Close cursor BEFORE processing

	categorizedEmails := make(map[string][]EmailSummary)

	// Now process all messages without holding cursor
	for _, msgInfo := range messagesToProcess {
		// Archived mail is only listed, never summarized
		if msgInfo.archived {
			categorizedEmails[msgInfo.category] = append(categorizedEmails[msgInfo.category], EmailSummary{
				MessageID:  msgInfo.id,
				Subject:    msgInfo.subject,
				From:       msgInfo.fromAddr,
				Category:   msgInfo.category,
				Summary:    fmt.Sprintf("**%s** - %s", msgInfo.subject, msgInfo.fromAddr),
				WebLink:    msgInfo.webLink,
				ReceivedAt: msgInfo.receivedAt,
				Archived:   true,
			})
			continue
		}

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

		// No cached summary - need to fetch message body for summarization.
		// While a music app runs, summaries are deferred, so don't fetch.
		var msg *graph.Message
		if classify.StudioBusy() {
			// falls through to the simple summary below
		} else if msgInfo.internetMessageId.Valid && msgInfo.internetMessageId.String != "" {
			msg, _ = d.client.GetMessageByInternetMessageId(ctx, msgInfo.internetMessageId.String)
		}
		if msg == nil && !classify.StudioBusy() {
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

	inbox, filed := digest.Split()
	categoryBreakdown := make(map[string]int)
	for _, emails := range digest.Categories {
		totalEmails += len(emails)
	}

	for category, emails := range inbox {
		count := len(emails)
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

	metaSummary := fmt.Sprintf("📧 **%d emails** received: %d still in inbox, %d filed away",
		totalEmails, totalEmails-len(filed), len(filed))

	if urgentCount > 0 {
		metaSummary += fmt.Sprintf(" (%d urgent/high priority)", urgentCount)
	}

	metaSummary += "\n\n**Still in inbox, by category:**\n"

	// Get sorted categories
	sortedCategories, _ := d.GetCategoryImportance(ctx)

	for _, category := range OrderedCategories(sortedCategories, inbox) {
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

// summaryFilePath returns the companion summary file path for a digest output.
// e.g. "/tmp/digest.md" -> "/tmp/digest.summary.md".
// If the input has no ".md" suffix, ".summary.md" is appended verbatim.
func summaryFilePath(digestPath string) string {
	if strings.HasSuffix(digestPath, ".md") {
		return strings.TrimSuffix(digestPath, ".md") + ".summary.md"
	}
	return digestPath + ".summary.md"
}

// SummaryFilePath exposes summaryFilePath for callers who want to preview
// where the companion file will be written.
func SummaryFilePath(digestPath string) string {
	return summaryFilePath(digestPath)
}

// WriteSummaryFile writes a machine-readable companion file alongside the
// main digest output. The digest (with its Categories populated) is bucketed
// into "Needs Attention" / "People & Clients" / "Archived" / "Stats" using
// per-message priority inferred from category + extracted metadata.
func (d *DigestService) WriteSummaryFile(ctx context.Context, digest *DigestSummary, digestPath string) error {
	if digestPath == "" {
		return nil
	}
	path := summaryFilePath(digestPath)

	type scored struct {
		email    EmailSummary
		priority string
	}

	var needsAttention []scored
	var peopleClients []scored
	archivedCounts := make(map[string]int)
	needsReview := 0
	archivedTotal := 0
	inboxRemaining := 0
	totalProcessed := 0

	for category, emails := range digest.Categories {
		for _, email := range emails {
			totalProcessed++
			if email.Archived {
				archivedCounts[category]++
				archivedTotal++
			} else {
				inboxRemaining++
			}
			if category == "Needs Review" {
				needsReview++
			}
			if email.Archived {
				continue // Jev judged it doesn't need attention
			}

			metadata := classify.ExtractMetadata(email.Subject, "")
			priority := classify.InferPriority(category, metadata)

			s := scored{email: email, priority: priority}

			switch category {
			case "People", "Clients":
				if priority == "urgent" || priority == "high" {
					needsAttention = append(needsAttention, s)
				} else {
					peopleClients = append(peopleClients, s)
				}
			case "Projects":
				if priority == "urgent" || priority == "high" {
					needsAttention = append(needsAttention, s)
				}
			case "Finance":
				// Finance receipts land at priority "low" (has amount, no change).
				// Anything urgent/high (overdue, cost increase) is attention-worthy.
				if priority == "urgent" || priority == "high" {
					needsAttention = append(needsAttention, s)
				}
			}
		}
	}

	// Sort each bucket by received time (newest first) for stable output.
	sort.Slice(needsAttention, func(i, j int) bool {
		return needsAttention[i].email.ReceivedAt.After(needsAttention[j].email.ReceivedAt)
	})
	sort.Slice(peopleClients, func(i, j int) bool {
		return peopleClients[i].email.ReceivedAt.After(peopleClients[j].email.ReceivedAt)
	})

	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString(fmt.Sprintf("date: %s\n", digest.EndTime.Format("2006-01-02")))
	b.WriteString(fmt.Sprintf("window: %s-%s\n",
		digest.StartTime.Format("15:04"),
		digest.EndTime.Format("15:04")))
	b.WriteString("---\n\n")

	b.WriteString("## 🔴 Needs Attention\n")
	if len(needsAttention) == 0 {
		b.WriteString("_None_\n")
	} else {
		for _, s := range needsAttention {
			b.WriteString(formatSummaryLine(s.email))
		}
	}
	b.WriteString("\n")

	b.WriteString("## 👤 People & Clients\n")
	if len(peopleClients) == 0 {
		b.WriteString("_None_\n")
	} else {
		for _, s := range peopleClients {
			b.WriteString(formatSummaryLine(s.email))
		}
	}
	b.WriteString("\n")

	b.WriteString("## 🗃 Archived\n")
	if len(archivedCounts) == 0 {
		b.WriteString("_None_\n")
	} else {
		// Deterministic order by category name.
		cats := make([]string, 0, len(archivedCounts))
		for c := range archivedCounts {
			cats = append(cats, c)
		}
		sort.Strings(cats)
		parts := make([]string, 0, len(cats))
		for _, c := range cats {
			parts = append(parts, fmt.Sprintf("%s: %d", c, archivedCounts[c]))
		}
		b.WriteString(strings.Join(parts, " | "))
		b.WriteString("\n")
	}
	b.WriteString("\n")

	b.WriteString("## 📊 Stats\n")
	b.WriteString(fmt.Sprintf("Total processed: %d | Archived: %d | Inbox remaining: %d | Needs review: %d\n",
		totalProcessed, archivedTotal, inboxRemaining, needsReview))

	return os.WriteFile(path, []byte(b.String()), 0644)
}

// formatSummaryLine renders one email as a single-bullet line for the
// summary file: "- **From**: ... | **Subject**: ... | [Open](...)".
func formatSummaryLine(email EmailSummary) string {
	from := email.From
	subject := strings.TrimSpace(email.Subject)
	if subject == "" {
		subject = "(no subject)"
	}
	return fmt.Sprintf("- **From**: %s | **Subject**: %s | **ID**: %s | [Open](%s)\n",
		from, subject, email.MessageID, email.WebLink)
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
