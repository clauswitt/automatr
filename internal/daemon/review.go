package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/clauswitt/automatr/internal/archive"
	"github.com/clauswitt/automatr/internal/classify"
	"github.com/clauswitt/automatr/internal/graph"
	"github.com/clauswitt/automatr/internal/summarize"
)

type ReviewServer struct {
	db         *sql.DB
	client     *graph.Client
	classifier *classify.Classifier
	digestSvc  *summarize.DigestService
	archiver   *archive.Archiver
}

type ReviewMessage struct {
	ID             string
	Subject        string
	From           string
	Date           string
	Category       string
	Priority       string
	Confidence     float64
	Method         string
	Metadata       map[string]string
	IsWhitelisted  bool
	WhitelistInfo  string
}

func NewReviewServer(db *sql.DB, client *graph.Client, classifier *classify.Classifier) *ReviewServer {
	return &ReviewServer{
		db:         db,
		client:     client,
		classifier: classifier,
		digestSvc:  summarize.NewDigestService(db, classifier, client),
	}
}

func (r *ReviewServer) HandleReviewUI(w http.ResponseWriter, req *http.Request) {
	page, _ := strconv.Atoi(req.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	perPage := 20

	// Get messages from database
	ctx := req.Context()
	messages, total, err := r.getReviewMessages(ctx, page, perPage)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	funcMap := template.FuncMap{
		"add": func(a, b int) int { return a + b },
		"sub": func(a, b int) int { return a - b },
	}

	tmpl := template.Must(template.New("review").Funcs(funcMap).Parse(reviewTemplate))
	data := struct {
		Messages   []ReviewMessage
		Page       int
		PerPage    int
		Total      int
		TotalPages int
		HasPrev    bool
		HasNext    bool
	}{
		Messages:   messages,
		Page:       page,
		PerPage:    perPage,
		Total:      total,
		TotalPages: (total + perPage - 1) / perPage,
		HasPrev:    page > 1,
		HasNext:    page*perPage < total,
	}

	tmpl.Execute(w, data)
}

func (r *ReviewServer) HandleWhitelistAdd(w http.ResponseWriter, req *http.Request) {
	if req.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	pattern := req.FormValue("pattern")
	patternType := req.FormValue("pattern_type")
	categoriesStr := req.FormValue("categories")
	autoArchive := req.FormValue("auto_archive") == "1"
	page := req.FormValue("page")
	if page == "" {
		page = "1"
	}

	var categories []string
	if categoriesStr != "" {
		if err := json.Unmarshal([]byte(categoriesStr), &categories); err != nil {
			// Try comma-separated
			categories = []string{categoriesStr}
		}
	}

	wm := r.classifier.GetWhitelistManager()
	if err := wm.AddWhitelist(req.Context(), pattern, patternType, categories, autoArchive); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, req, "/review?page="+page, http.StatusSeeOther)
}

func (r *ReviewServer) HandleWhitelistRemove(w http.ResponseWriter, req *http.Request) {
	if req.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id, err := strconv.Atoi(req.FormValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}

	wm := r.classifier.GetWhitelistManager()
	if err := wm.RemoveWhitelist(req.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, req, "/whitelists", http.StatusSeeOther)
}

func (r *ReviewServer) HandleWhitelistUpdate(w http.ResponseWriter, req *http.Request) {
	if req.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id, err := strconv.Atoi(req.FormValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}

	// Parse categories from form (checkboxes)
	req.ParseForm()
	categories := req.Form["categories"]
	autoArchive := req.FormValue("auto_archive") == "1"

	wm := r.classifier.GetWhitelistManager()
	if err := wm.UpdateWhitelist(req.Context(), id, categories, autoArchive); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, req, "/whitelists", http.StatusSeeOther)
}

func (r *ReviewServer) HandleReclassify(w http.ResponseWriter, req *http.Request) {
	if req.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	msgID := req.FormValue("msg")
	page := req.FormValue("page")
	if page == "" {
		page = "1"
	}

	if msgID == "" {
		http.Error(w, "Missing msg parameter", http.StatusBadRequest)
		return
	}

	ctx := req.Context()

	// Fetch email from Graph API
	msg, err := r.client.GetMessage(ctx, msgID)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to fetch email: %v", err), http.StatusInternalServerError)
		return
	}

	// Prepare email input
	bodyText := msg.Body.Content
	if len(bodyText) > 1000 {
		bodyText = bodyText[:1000]
	}

	emailInput := classify.EmailInput{
		From:         msg.From.EmailAddress.Address,
		Subject:      msg.Subject,
		Body:         bodyText,
		Headers:      make(map[string]string),
		ToRecipients: len(msg.ToRecipients),
	}

	// Force re-classification
	result, err := r.classifier.Classify(ctx, emailInput)
	if err != nil {
		http.Error(w, fmt.Sprintf("Classification failed: %v", err), http.StatusInternalServerError)
		return
	}

	// Check whitelist restrictions
	wm := r.classifier.GetWhitelistManager()
	whitelist, err := wm.CheckWhitelist(ctx, msg.From.EmailAddress.Address)
	if err == nil && whitelist != nil {
		// Apply whitelist category restrictions
		if len(whitelist.AllowedCategories) > 0 {
			// Check if current category is allowed
			allowed := false
			for _, cat := range whitelist.AllowedCategories {
				if cat == result.Category {
					allowed = true
					break
				}
			}
			// If not allowed, use the first allowed category
			if !allowed {
				result.Category = whitelist.AllowedCategories[0]
			}
		}
	}

	// Update database
	_, err = r.db.ExecContext(ctx, `
		UPDATE messages
		SET category = ?
		WHERE id = ?
	`, result.Category, msgID)

	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to update database: %v", err), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, req, "/review?page="+page, http.StatusSeeOther)
}

func (r *ReviewServer) HandleWhitelistsUI(w http.ResponseWriter, req *http.Request) {
	wm := r.classifier.GetWhitelistManager()
	whitelists, err := wm.ListWhitelists(req.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	funcMap := template.FuncMap{
		"containsCategory": func(categories []string, category string) bool {
			for _, c := range categories {
				if c == category {
					return true
				}
			}
			return false
		},
	}

	tmpl := template.Must(template.New("whitelists").Funcs(funcMap).Parse(whitelistsTemplate))
	tmpl.Execute(w, whitelists)
}

func (r *ReviewServer) HandleLearnedRulesUI(w http.ResponseWriter, req *http.Request) {
	rows, err := r.db.QueryContext(req.Context(), `
		SELECT id, pattern, pattern_type, category, priority, confidence, feedback_count, created_at, last_applied
		FROM learned_rules
		ORDER BY feedback_count DESC, confidence DESC
	`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type LearnedRule struct {
		ID            int
		Pattern       string
		PatternType   string
		Category      string
		Priority      string
		Confidence    float64
		FeedbackCount int
		CreatedAt     string
		LastApplied   *string
	}

	var rules []LearnedRule
	for rows.Next() {
		var rule LearnedRule
		if err := rows.Scan(&rule.ID, &rule.Pattern, &rule.PatternType, &rule.Category, &rule.Priority,
			&rule.Confidence, &rule.FeedbackCount, &rule.CreatedAt, &rule.LastApplied); err != nil {
			continue
		}
		rules = append(rules, rule)
	}

	funcMap := template.FuncMap{
		"mul": func(a, b float64) float64 { return a * b },
	}

	tmpl := template.Must(template.New("learned-rules").Funcs(funcMap).Parse(learnedRulesTemplate))
	tmpl.Execute(w, rules)
}

func (r *ReviewServer) HandleLearnedRuleDelete(w http.ResponseWriter, req *http.Request) {
	if req.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id, err := strconv.Atoi(req.FormValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}

	_, err = r.db.ExecContext(req.Context(), `DELETE FROM learned_rules WHERE id = ?`, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, req, "/learned-rules", http.StatusSeeOther)
}

func (r *ReviewServer) HandleLearnedRuleUpdate(w http.ResponseWriter, req *http.Request) {
	if req.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id, err := strconv.Atoi(req.FormValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}

	category := req.FormValue("category")
	priority := req.FormValue("priority")

	_, err = r.db.ExecContext(req.Context(), `
		UPDATE learned_rules
		SET category = ?, priority = ?
		WHERE id = ?
	`, category, priority, id)

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, req, "/learned-rules", http.StatusSeeOther)
}

// Category Management Handlers

func (r *ReviewServer) HandleCategoriesUI(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	categoryMgr := r.classifier.GetCategoryManager()
	categories, err := categoryMgr.ListCategories(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	tmpl := template.Must(template.New("categories").Parse(categoriesTemplate))
	data := struct {
		Categories []classify.Category
	}{
		Categories: categories,
	}

	tmpl.Execute(w, data)
}

func (r *ReviewServer) HandleCategoryAdd(w http.ResponseWriter, req *http.Request) {
	if req.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	name := req.FormValue("name")
	description := req.FormValue("description")
	summarizationPrompt := req.FormValue("summarization_prompt")
	keepInInbox := req.FormValue("keep_in_inbox") == "1"
	priorityDefault := req.FormValue("priority_default")

	if name == "" {
		http.Error(w, "Category name is required", http.StatusBadRequest)
		return
	}

	categoryMgr := r.classifier.GetCategoryManager()
	err := categoryMgr.AddCategory(req.Context(), name, description, summarizationPrompt, keepInInbox, priorityDefault)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, req, "/categories", http.StatusSeeOther)
}

func (r *ReviewServer) HandleCategoryUpdate(w http.ResponseWriter, req *http.Request) {
	if req.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id, err := strconv.Atoi(req.FormValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}

	description := req.FormValue("description")
	summarizationPrompt := req.FormValue("summarization_prompt")
	keepInInbox := req.FormValue("keep_in_inbox") == "1"
	priorityDefault := req.FormValue("priority_default")

	categoryMgr := r.classifier.GetCategoryManager()
	err = categoryMgr.UpdateCategory(req.Context(), id, description, summarizationPrompt, keepInInbox, priorityDefault)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, req, "/categories", http.StatusSeeOther)
}

func (r *ReviewServer) HandleCategoryDelete(w http.ResponseWriter, req *http.Request) {
	if req.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id, err := strconv.Atoi(req.FormValue("id"))
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}

	categoryMgr := r.classifier.GetCategoryManager()
	err = categoryMgr.DeleteCategory(req.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	http.Redirect(w, req, "/categories", http.StatusSeeOther)
}

// Digest Management Handlers

func (r *ReviewServer) HandleDigestUI(w http.ResponseWriter, req *http.Request) {
	tmpl := template.Must(template.New("digest").Parse(digestUITemplate))
	tmpl.Execute(w, nil)
}

func (r *ReviewServer) HandleCreateDigest(w http.ResponseWriter, req *http.Request) {
	if req.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := req.Context()

	// Parse timespan parameter
	timespan := req.FormValue("timespan")
	var startTime time.Time
	endTime := time.Now()

	if timespan == "auto" {
		// Use "since last digest" or default to 24 hours
		lastEnd, err := r.digestSvc.GetLastDigestEndTime(ctx)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if lastEnd != nil {
			startTime = *lastEnd
		} else {
			startTime = endTime.Add(-24 * time.Hour)
		}
	} else {
		// Parse custom timespan
		duration, err := time.ParseDuration(timespan)
		if err != nil {
			http.Error(w, "Invalid timespan", http.StatusBadRequest)
			return
		}
		startTime = endTime.Add(-duration)
	}

	// Archiving happens during email processing (archive.Archiver), so the
	// digest already sees post-archive webLinks.
	digest, err := r.digestSvc.CreateDigest(ctx, startTime, endTime)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Generate meta summary
	_, err = r.digestSvc.GenerateMetaSummary(ctx, digest)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Redirect to view the digest
	http.Redirect(w, req, fmt.Sprintf("/digest/view?id=%d", digest.ID), http.StatusSeeOther)
}

func (r *ReviewServer) HandleViewDigest(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	digestID, err := strconv.Atoi(req.URL.Query().Get("id"))
	if err != nil {
		http.Error(w, "Invalid digest ID", http.StatusBadRequest)
		return
	}

	// Load digest from database
	var startTime, endTime time.Time
	var metaSummary sql.NullString

	err = r.db.QueryRowContext(ctx, `
		SELECT start_time, end_time, meta_summary
		FROM digest_summaries
		WHERE id = ?
	`, digestID).Scan(&startTime, &endTime, &metaSummary)

	if err != nil {
		http.Error(w, "Digest not found", http.StatusNotFound)
		return
	}

	digest := &summarize.DigestSummary{
		ID:          digestID,
		StartTime:   startTime,
		EndTime:     endTime,
		MetaSummary: metaSummary.String,
		Categories:  make(map[string][]summarize.EmailSummary),
	}

	// Load all emails for this digest with preserved webLinks
	rows, err := r.db.QueryContext(ctx, `
		SELECT m.id, m.subject, m.from_addr, m.category, m.received_at,
		       COALESCE(de.weblink, m.raw_link) as weblink,
		       m.internet_message_id,
		       COALESCE(es.summary, '') as summary,
		       m.archived_at IS NOT NULL
		FROM messages m
		LEFT JOIN email_summaries es ON m.id = es.message_id
		LEFT JOIN digest_emails de ON de.digest_id = ? AND de.message_id = m.id
		WHERE datetime(m.received_at) >= datetime(?) AND datetime(m.received_at) < datetime(?)
		ORDER BY m.received_at DESC
	`, digestID, startTime.Format(time.RFC3339), endTime.Format(time.RFC3339))

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var msgID, subject, fromAddr, category, webLink, summary string
		var internetMessageId sql.NullString
		var receivedAt time.Time
		var archived bool

		if err := rows.Scan(&msgID, &subject, &fromAddr, &category, &receivedAt, &webLink, &internetMessageId, &summary, &archived); err != nil {
			continue
		}

		if summary == "" {
			summary = subject // Fallback to subject if no summary
		}

		digest.Categories[category] = append(digest.Categories[category], summarize.EmailSummary{
			MessageID:  msgID,
			Subject:    subject,
			From:       fromAddr,
			Category:   category,
			Summary:    summary,
			WebLink:    webLink, // From digest_emails table (preserved at digest creation time)
			ReceivedAt: receivedAt,
			Archived:   archived,
		})
	}

	// Get category importance order
	categoryOrder, _ := r.digestSvc.GetCategoryImportance(ctx)

	inbox, filed := digest.Split()
	data := struct {
		Digest        *summarize.DigestSummary
		CategoryOrder []string
		Inbox         map[string][]summarize.EmailSummary
		FiledAway     []summarize.EmailSummary
	}{
		Digest:        digest,
		CategoryOrder: summarize.OrderedCategories(categoryOrder, inbox),
		Inbox:         inbox,
		FiledAway:     filed,
	}

	tmpl := template.Must(template.New("digest-view").Parse(digestViewTemplate))
	tmpl.Execute(w, data)
}

func (r *ReviewServer) HandleExportDigestMarkdown(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	digestID, err := strconv.Atoi(req.URL.Query().Get("id"))
	if err != nil {
		http.Error(w, "Invalid digest ID", http.StatusBadRequest)
		return
	}

	// Load digest (reuse logic from HandleViewDigest)
	var startTime, endTime time.Time
	var metaSummary sql.NullString

	err = r.db.QueryRowContext(ctx, `
		SELECT start_time, end_time, meta_summary
		FROM digest_summaries
		WHERE id = ?
	`, digestID).Scan(&startTime, &endTime, &metaSummary)

	if err != nil {
		http.Error(w, "Digest not found", http.StatusNotFound)
		return
	}

	digest := &summarize.DigestSummary{
		ID:          digestID,
		StartTime:   startTime,
		EndTime:     endTime,
		MetaSummary: metaSummary.String,
		Categories:  make(map[string][]summarize.EmailSummary),
	}

	// Load emails
	rows, err := r.db.QueryContext(ctx, `
		SELECT m.id, m.subject, m.from_addr, m.category, m.received_at, m.raw_link, m.internet_message_id,
		       COALESCE(es.summary, '') as summary,
		       m.archived_at IS NOT NULL
		FROM messages m
		LEFT JOIN email_summaries es ON m.id = es.message_id
		WHERE datetime(m.received_at) >= datetime(?) AND datetime(m.received_at) < datetime(?)
		ORDER BY m.received_at DESC
	`, startTime.Format(time.RFC3339), endTime.Format(time.RFC3339))

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var msgID, subject, fromAddr, category, webLink, summary string
		var internetMessageId sql.NullString
		var receivedAt time.Time
		var archived bool

		if err := rows.Scan(&msgID, &subject, &fromAddr, &category, &receivedAt, &webLink, &internetMessageId, &summary, &archived); err != nil {
			continue
		}

		// Generate stable search-based link using internetMessageId (never changes regardless of folder)
		currentWebLink := webLink // fallback
		if internetMessageId.Valid && internetMessageId.String != "" {
			// Remove angle brackets if present
			msgID := internetMessageId.String
			if len(msgID) > 0 && msgID[0] == '<' {
				msgID = msgID[1:]
			}
			if len(msgID) > 0 && msgID[len(msgID)-1] == '>' {
				msgID = msgID[:len(msgID)-1]
			}
			currentWebLink = fmt.Sprintf("https://outlook.office.com/mail/search?q=msgid:%s", msgID)
		}

		if summary == "" {
			summary = subject
		}

		digest.Categories[category] = append(digest.Categories[category], summarize.EmailSummary{
			MessageID:  msgID,
			Subject:    subject,
			From:       fromAddr,
			Category:   category,
			Summary:    summary,
			WebLink:    currentWebLink,
			ReceivedAt: receivedAt,
			Archived:   archived,
		})
	}

	// Generate markdown
	categoryOrder, _ := r.digestSvc.GetCategoryImportance(ctx)

	var markdown strings.Builder
	markdown.WriteString(fmt.Sprintf("# Email Digest\n\n"))
	markdown.WriteString(fmt.Sprintf("**Period:** %s to %s\n\n", startTime.Format("2006-01-02 15:04"), endTime.Format("2006-01-02 15:04")))

	if digest.MetaSummary != "" {
		markdown.WriteString(digest.MetaSummary)
		markdown.WriteString("\n\n---\n\n")
	}

	summarize.WriteMarkdownSections(&markdown, digest, categoryOrder)

	// Set headers for download
	w.Header().Set("Content-Type", "text/markdown")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"digest-%d.md\"", digestID))

	w.Write([]byte(markdown.String()))
}

func (r *ReviewServer) getReviewMessages(ctx context.Context, page, perPage int) ([]ReviewMessage, int, error) {
	// Get total count
	var total int
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages`).Scan(&total)
	if err != nil {
		return nil, 0, err
	}

	// Get paginated messages
	offset := (page - 1) * perPage
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, subject, from_addr, received_at, category
		FROM messages
		ORDER BY received_at DESC
		LIMIT ? OFFSET ?
	`, perPage, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	wm := r.classifier.GetWhitelistManager()
	var messages []ReviewMessage

	for rows.Next() {
		var msg ReviewMessage
		if err := rows.Scan(&msg.ID, &msg.Subject, &msg.From, &msg.Date, &msg.Category); err != nil {
			continue
		}

		// Check if whitelisted
		wl, _ := wm.CheckWhitelist(ctx, msg.From)
		if wl != nil {
			msg.IsWhitelisted = true
			if len(wl.AllowedCategories) > 0 {
				msg.WhitelistInfo = fmt.Sprintf("Whitelisted (%s only)", wl.AllowedCategories)
			} else {
				msg.WhitelistInfo = "Whitelisted (all categories)"
			}
		}

		messages = append(messages, msg)
	}

	return messages, total, rows.Err()
}

// StartScheduler starts a background job that processes emails on a schedule
func (r *ReviewServer) StartScheduler(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute) // Check every 5 minutes
	defer ticker.Stop()

	log.Println("Email processing scheduler started (interval: 5 minutes)")

	// Run once immediately
	r.processEmails(ctx)

	for {
		select {
		case <-ctx.Done():
			log.Println("Email processing scheduler stopped")
			return
		case <-ticker.C:
			r.processEmails(ctx)
		}
	}
}

// archive applies the archive policy; without an archiver nothing is moved.
func (r *ReviewServer) archive(ctx context.Context, msg graph.Message, category string, triage *classify.Triage) {
	if r.archiver == nil {
		return
	}
	decision, err := r.archiver.Process(ctx, msg, category, triage)
	if err != nil {
		log.Printf("Archive failed for %s: %v", msg.Subject, err)
		return
	}
	if decision.Archive {
		verb := "Auto-archived"
		if !r.archiver.Live() {
			verb = "Would archive"
		}
		log.Printf("✓ %s: %s (%s; %s)", verb, msg.Subject, category, decision.Reason)
	}
}

func (r *ReviewServer) processEmails(ctx context.Context) {
	log.Println("Processing emails...")

	// Fetch ONLY from inbox (don't process already-archived messages)
	since := time.Now().Add(-48 * time.Hour)
	messages, err := r.client.ListInboxMessagesSince(ctx, since)
	if err != nil {
		log.Printf("Failed to fetch inbox messages: %v", err)
		return
	}

	log.Printf("Found %d messages in inbox from last 48 hours", len(messages))

	processedCount := 0
	newCount := 0

	for _, msg := range messages {
		// Check if email already exists in database (by internetMessageId OR id)
		var existingID, existingCategory string
		var existingInternetMsgID sql.NullString

		// Try by internet_message_id first
		err := r.db.QueryRowContext(ctx, `SELECT id, category, internet_message_id FROM messages WHERE internet_message_id = ? AND internet_message_id != ''`, msg.InternetMessageId).Scan(&existingID, &existingCategory, &existingInternetMsgID)

		// Fallback to id if not found by internet_message_id
		if err != nil {
			err = r.db.QueryRowContext(ctx, `SELECT id, category, internet_message_id FROM messages WHERE id = ?`, msg.ID).Scan(&existingID, &existingCategory, &existingInternetMsgID)
		}

		if err == nil {
			// Message already exists
			// Update internet_message_id if missing
			if !existingInternetMsgID.Valid || existingInternetMsgID.String == "" {
				_, _ = r.db.ExecContext(ctx, `UPDATE messages SET internet_message_id = ? WHERE id = ?`, msg.InternetMessageId, existingID)
			}

			// Update ID and webLink if message was moved
			if existingID != msg.ID {
				_, _ = r.db.ExecContext(ctx, `UPDATE messages SET id = ?, raw_link = ? WHERE id = ?`,
					msg.ID, msg.WebLink, existingID)
				log.Printf("Updated message ID for %s (moved from %s to %s)", msg.Subject, existingID, msg.ID)
			}

			// Re-decide archiving (e.g. an OTP that has since been read)
			r.archive(ctx, msg, existingCategory, nil)
			continue
		}

		// Email is new, classify it
		bodyText := msg.Body.Content
		if len(bodyText) > 4000 {
			bodyText = bodyText[:4000]
		}

		email := classify.EmailInput{
			From:    msg.From.EmailAddress.Address,
			Subject: msg.Subject,
			Body:    bodyText,
		}

		result, err := r.classifier.Classify(ctx, email)
		if err != nil {
			log.Printf("Failed to classify email %s: %v", msg.ID, err)
			continue
		}

		// Store in database
		_, err = r.db.ExecContext(ctx, `
			INSERT INTO messages (id, subject, from_addr, category, received_at, raw_link, internet_message_id)
			VALUES (?, ?, ?, ?, ?, ?, ?)
		`, msg.ID, msg.Subject, msg.From.EmailAddress.Address, result.Category, msg.ReceivedDateTime, msg.WebLink, msg.InternetMessageId)

		if err != nil {
			log.Printf("Failed to store email %s: %v", msg.ID, err)
			continue
		}

		// Auto-archive if it doesn't need personal attention
		r.archive(ctx, msg, result.Category, result.Triage)

		newCount++
		processedCount++
	}

	if newCount > 0 {
		log.Printf("✓ Processed %d new emails", newCount)
	} else {
		log.Printf("✓ No new emails to process (all up to date)")
	}
}

const reviewTemplate = `
<!DOCTYPE html>
<html>
<head>
    <title>Email Review</title>
    <style>
        body {
            font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
            margin: 0;
            padding: 20px;
            background: #f5f5f5;
        }
        .container { max-width: 1200px; margin: 0 auto; }
        h1 { color: #333; }
        .stats {
            background: #fff;
            padding: 15px;
            border-radius: 8px;
            margin-bottom: 20px;
            box-shadow: 0 1px 3px rgba(0,0,0,0.1);
        }
        .email-card {
            background: white;
            padding: 15px;
            margin-bottom: 10px;
            border-radius: 8px;
            box-shadow: 0 1px 3px rgba(0,0,0,0.1);
        }
        .email-card.whitelisted {
            border-left: 4px solid #28a745;
        }
        .email-subject { font-weight: 600; font-size: 16px; margin-bottom: 5px; }
        .email-from { color: #666; font-size: 14px; margin-bottom: 10px; }
        .email-meta { display: flex; gap: 15px; margin-bottom: 10px; font-size: 14px; }
        .tag {
            padding: 4px 8px;
            border-radius: 4px;
            font-size: 12px;
            font-weight: 500;
        }
        .tag-category { background: #e3f2fd; color: #1976d2; }
        .tag-priority { background: #fff3e0; color: #f57c00; }
        .tag-whitelisted { background: #e8f5e9; color: #2e7d32; }
        .actions {
            display: flex;
            gap: 10px;
            margin-top: 10px;
        }
        .btn {
            padding: 6px 12px;
            border: none;
            border-radius: 4px;
            cursor: pointer;
            font-size: 13px;
            text-decoration: none;
            display: inline-block;
        }
        .btn-correct { background: #28a745; color: white; }
        .btn-wrong { background: #ffc107; color: #333; }
        .btn-reclassify { background: #6f42c1; color: white; }
        .btn-whitelist { background: #17a2b8; color: white; }
        .btn-whitelist-domain { background: #6c757d; color: white; }
        .pagination {
            display: flex;
            justify-content: center;
            gap: 10px;
            margin: 20px 0;
        }
        .pagination a {
            padding: 8px 16px;
            background: white;
            border-radius: 4px;
            text-decoration: none;
            color: #333;
        }
        .pagination a.active { background: #007bff; color: white; }
        nav {
            background: white;
            padding: 10px 20px;
            margin-bottom: 20px;
            border-radius: 8px;
            box-shadow: 0 1px 3px rgba(0,0,0,0.1);
        }
        nav a {
            margin-right: 15px;
            color: #007bff;
            text-decoration: none;
        }
    </style>
</head>
<body>
    <div class="container">
        <nav>
            <a href="/review">📧 Review</a>
            <a href="/whitelists">✅ Whitelists</a>
            <a href="/learned-rules">📚 Learned Rules</a>
            <a href="/categories">🏷️ Categories</a>
            <a href="/stats">📊 Stats</a>
            <a href="/">🏠 Home</a>
        </nav>

        <h1>📧 Email Review</h1>

        <div class="stats">
            Showing {{.Page}} of {{.TotalPages}} ({{.Total}} total emails)
        </div>

        {{range .Messages}}
        <div class="email-card {{if .IsWhitelisted}}whitelisted{{end}}">
            <div class="email-subject">{{.Subject}}</div>
            <div class="email-from">From: {{.From}}</div>
            <div class="email-meta">
                <span class="tag tag-category">📂 {{.Category}}</span>
                {{if .IsWhitelisted}}
                <span class="tag tag-whitelisted">✅ {{.WhitelistInfo}}</span>
                {{end}}
            </div>
            <div class="actions">
                <a href="/correct?msg={{.ID}}&page={{$.Page}}" class="btn btn-correct">✓ Correct</a>
                <a href="/wrong?msg={{.ID}}&page={{$.Page}}" class="btn btn-wrong">✎ Wrong</a>
                <button onclick="reclassify('{{.ID}}', {{$.Page}})" class="btn btn-reclassify">🔄 Re-classify</button>
                {{if not .IsWhitelisted}}
                <button onclick="whitelist('{{.From}}', 'email', {{$.Page}})" class="btn btn-whitelist">✅ Whitelist Email</button>
                <button onclick="whitelistDomain('{{.From}}', {{$.Page}})" class="btn btn-whitelist-domain">✅ Whitelist Domain</button>
                {{end}}
            </div>
        </div>
        {{end}}

        <div class="pagination">
            {{if .HasPrev}}
            <a href="/review?page={{sub .Page 1}}">← Previous</a>
            {{end}}
            <span>Page {{.Page}} of {{.TotalPages}}</span>
            {{if .HasNext}}
            <a href="/review?page={{add .Page 1}}">Next →</a>
            {{end}}
        </div>
    </div>

    <script>
    function reclassify(msgId, page) {
        if (confirm('Re-classify this email using current rules?')) {
            const form = document.createElement('form');
            form.method = 'POST';
            form.action = '/reclassify';
            form.innerHTML = '<input name="msg" value="' + msgId + '">' +
                           '<input name="page" value="' + page + '">';
            document.body.appendChild(form);
            form.submit();
        }
    }

    function whitelist(email, type, page) {
        if (confirm('Whitelist ' + email + '?')) {
            const form = document.createElement('form');
            form.method = 'POST';
            form.action = '/whitelist/add?page=' + page;
            form.innerHTML = '<input name="pattern" value="' + email + '">' +
                           '<input name="pattern_type" value="' + type + '">' +
                           '<input name="page" value="' + page + '">';
            document.body.appendChild(form);
            form.submit();
        }
    }

    function whitelistDomain(email, page) {
        const domain = email.split('@')[1];
        if (confirm('Whitelist entire domain ' + domain + '?')) {
            const form = document.createElement('form');
            form.method = 'POST';
            form.action = '/whitelist/add?page=' + page;
            form.innerHTML = '<input name="pattern" value="' + domain + '">' +
                           '<input name="pattern_type" value="domain">' +
                           '<input name="page" value="' + page + '">';
            document.body.appendChild(form);
            form.submit();
        }
    }
    </script>
</body>
</html>
`

const learnedRulesTemplate = `
<!DOCTYPE html>
<html>
<head>
    <title>Learned Rules</title>
    <style>
        body {
            font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
            margin: 0;
            padding: 20px;
            background: #f5f5f5;
        }
        .container { max-width: 1200px; margin: 0 auto; }
        h1 { color: #333; }
        nav {
            background: white;
            padding: 10px 20px;
            margin-bottom: 20px;
            border-radius: 8px;
            box-shadow: 0 1px 3px rgba(0,0,0,0.1);
        }
        nav a {
            margin-right: 15px;
            color: #007bff;
            text-decoration: none;
        }
        .rule-card {
            background: white;
            padding: 15px;
            margin-bottom: 15px;
            border-radius: 8px;
            box-shadow: 0 1px 3px rgba(0,0,0,0.1);
        }
        .rule-header {
            display: flex;
            justify-content: space-between;
            align-items: center;
            margin-bottom: 10px;
        }
        .pattern { font-weight: 600; font-size: 16px; font-family: monospace; }
        .type-badge {
            padding: 4px 8px;
            background: #e3f2fd;
            color: #1976d2;
            border-radius: 4px;
            font-size: 12px;
        }
        .stats {
            margin: 10px 0;
            font-size: 14px;
            color: #666;
        }
        .stat-item {
            display: inline-block;
            margin-right: 20px;
        }
        .confidence-bar {
            height: 4px;
            background: #e0e0e0;
            border-radius: 2px;
            overflow: hidden;
            margin: 5px 0;
        }
        .confidence-fill {
            height: 100%;
            background: #4caf50;
        }
        .btn {
            padding: 6px 12px;
            border: none;
            border-radius: 4px;
            cursor: pointer;
            font-size: 13px;
            margin-right: 5px;
        }
        .btn-edit { background: #007bff; color: white; }
        .btn-remove { background: #dc3545; color: white; }
        .edit-form { display: none; margin-top: 15px; padding-top: 15px; border-top: 1px solid #eee; }
        .edit-form.active { display: block; }
        select, input {
            padding: 6px;
            margin-right: 10px;
            border: 1px solid #ddd;
            border-radius: 4px;
        }
    </style>
</head>
<body>
    <div class="container">
        <nav>
            <a href="/review">📧 Review</a>
            <a href="/whitelists">✅ Whitelists</a>
            <a href="/learned-rules">📚 Learned Rules</a>
            <a href="/categories">🏷️ Categories</a>
            <a href="/stats">📊 Stats</a>
            <a href="/">🏠 Home</a>
        </nav>

        <h1>📚 Learned Rules</h1>
        <p>These rules were automatically created from your feedback corrections.</p>

        {{if .}}
        {{range .}}
        <div class="rule-card">
            <div class="rule-header">
                <div>
                    <span class="pattern">{{.Pattern}}</span>
                    <span class="type-badge">{{.PatternType}}</span>
                </div>
                <div>
                    <button onclick="toggleEdit({{.ID}})" class="btn btn-edit">✎ Edit</button>
                    <form method="POST" action="/learned-rule/delete" style="display:inline">
                        <input type="hidden" name="id" value="{{.ID}}">
                        <button type="submit" class="btn btn-remove" onclick="return confirm('Delete this rule?')">Delete</button>
                    </form>
                </div>
            </div>

            <div id="view-{{.ID}}">
                <div class="stats">
                    <span class="stat-item"><strong>Category:</strong> {{.Category}}</span>
                    <span class="stat-item"><strong>Priority:</strong> {{.Priority}}</span>
                    <span class="stat-item"><strong>Corrections:</strong> {{.FeedbackCount}}×</span>
                    <span class="stat-item"><strong>Confidence:</strong> {{printf "%.0f" (mul .Confidence 100)}}%</span>
                </div>
                <div class="confidence-bar">
                    <div class="confidence-fill" style="width: {{printf "%.0f" (mul .Confidence 100)}}%"></div>
                </div>
                <div style="font-size: 12px; color: #999; margin-top: 5px;">
                    Created: {{.CreatedAt}}
                    {{if .LastApplied}}| Last used: {{.LastApplied}}{{end}}
                </div>
            </div>

            <div id="edit-{{.ID}}" class="edit-form">
                <form method="POST" action="/learned-rule/update">
                    <input type="hidden" name="id" value="{{.ID}}">
                    <label>Category:
                        <select name="category">
                            <option value="People" {{if eq .Category "People"}}selected{{end}}>People</option>
                            <option value="Clients" {{if eq .Category "Clients"}}selected{{end}}>Clients</option>
                            <option value="Finance" {{if eq .Category "Finance"}}selected{{end}}>Finance</option>
                            <option value="Auth" {{if eq .Category "Auth"}}selected{{end}}>Auth</option>
                            <option value="Marketing" {{if eq .Category "Marketing"}}selected{{end}}>Marketing</option>
                            <option value="Product Updates" {{if eq .Category "Product Updates"}}selected{{end}}>Product Updates</option>
                            <option value="Ops" {{if eq .Category "Ops"}}selected{{end}}>Ops</option>
                            <option value="Community" {{if eq .Category "Community"}}selected{{end}}>Community</option>
                            <option value="Projects" {{if eq .Category "Projects"}}selected{{end}}>Projects</option>
                            <option value="Later" {{if eq .Category "Later"}}selected{{end}}>Later</option>
                        </select>
                    </label>
                    <label>Priority:
                        <select name="priority">
                            <option value="urgent" {{if eq .Priority "urgent"}}selected{{end}}>Urgent</option>
                            <option value="high" {{if eq .Priority "high"}}selected{{end}}>High</option>
                            <option value="medium" {{if eq .Priority "medium"}}selected{{end}}>Medium</option>
                            <option value="low" {{if eq .Priority "low"}}selected{{end}}>Low</option>
                            <option value="spam" {{if eq .Priority "spam"}}selected{{end}}>Spam</option>
                        </select>
                    </label>
                    <button type="submit" class="btn btn-edit">Save</button>
                    <button type="button" onclick="toggleEdit({{.ID}})" class="btn">Cancel</button>
                </form>
            </div>
        </div>
        {{end}}
        {{else}}
        <div class="rule-card">
            <p>No learned rules yet. Process some feedback corrections to create rules!</p>
            <p>Run: <code>./automatr -process-feedback</code></p>
        </div>
        {{end}}
    </div>

    <script>
    function toggleEdit(id) {
        const viewEl = document.getElementById('view-' + id);
        const editEl = document.getElementById('edit-' + id);
        if (editEl.classList.contains('active')) {
            editEl.classList.remove('active');
            viewEl.style.display = 'block';
        } else {
            editEl.classList.add('active');
            viewEl.style.display = 'none';
        }
    }
    </script>
</body>
</html>
`

const whitelistsTemplate = `
<!DOCTYPE html>
<html>
<head>
    <title>Whitelists</title>
    <style>
        body {
            font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
            margin: 0;
            padding: 20px;
            background: #f5f5f5;
        }
        .container { max-width: 1200px; margin: 0 auto; }
        h1 { color: #333; }
        nav {
            background: white;
            padding: 10px 20px;
            margin-bottom: 20px;
            border-radius: 8px;
            box-shadow: 0 1px 3px rgba(0,0,0,0.1);
        }
        nav a {
            margin-right: 15px;
            color: #007bff;
            text-decoration: none;
        }
        .whitelist-card {
            background: white;
            padding: 15px;
            margin-bottom: 15px;
            border-radius: 8px;
            box-shadow: 0 1px 3px rgba(0,0,0,0.1);
        }
        .whitelist-header {
            display: flex;
            justify-content: space-between;
            align-items: center;
            margin-bottom: 10px;
        }
        .pattern { font-weight: 600; font-size: 16px; }
        .type-badge {
            padding: 4px 8px;
            background: #e3f2fd;
            color: #1976d2;
            border-radius: 4px;
            font-size: 12px;
        }
        .categories {
            margin: 10px 0;
        }
        .category-checkbox {
            display: inline-block;
            margin-right: 15px;
            margin-bottom: 5px;
        }
        .btn {
            padding: 6px 12px;
            border: none;
            border-radius: 4px;
            cursor: pointer;
            font-size: 13px;
            margin-right: 5px;
        }
        .btn-save { background: #28a745; color: white; }
        .btn-remove { background: #dc3545; color: white; }
        .edit-form { display: none; }
        .edit-form.active { display: block; }
    </style>
</head>
<body>
    <div class="container">
        <nav>
            <a href="/review">📧 Review</a>
            <a href="/whitelists">✅ Whitelists</a>
            <a href="/learned-rules">📚 Learned Rules</a>
            <a href="/categories">🏷️ Categories</a>
            <a href="/stats">📊 Stats</a>
            <a href="/">🏠 Home</a>
        </nav>

        <h1>✅ Whitelists</h1>

        {{range .}}
        <div class="whitelist-card">
            <div class="whitelist-header">
                <div>
                    <span class="pattern">{{.Pattern}}</span>
                    <span class="type-badge">{{.PatternType}}</span>
                </div>
                <div>
                    <button onclick="toggleEdit({{.ID}})" class="btn">✎ Edit</button>
                    <form method="POST" action="/whitelist/remove" style="display:inline">
                        <input type="hidden" name="id" value="{{.ID}}">
                        <button type="submit" class="btn btn-remove" onclick="return confirm('Remove this whitelist?')">Remove</button>
                    </form>
                </div>
            </div>

            <div id="view-{{.ID}}">
                <div><strong>Allowed Categories:</strong> {{if .AllowedCategories}}{{range $i, $cat := .AllowedCategories}}{{if $i}}, {{end}}{{$cat}}{{end}}{{else}}All categories{{end}}</div>
                <div><strong>Auto Archive:</strong> {{if .AutoArchive}}Yes{{else}}No{{end}}</div>
            </div>

            <div id="edit-{{.ID}}" class="edit-form">
                <form method="POST" action="/whitelist/update">
                    <input type="hidden" name="id" value="{{.ID}}">
                    <div class="categories">
                        <strong>Allowed Categories (leave all unchecked for "all"):</strong><br>
                        <label class="category-checkbox"><input type="checkbox" name="categories" value="People" {{if containsCategory .AllowedCategories "People"}}checked{{end}}> People</label>
                        <label class="category-checkbox"><input type="checkbox" name="categories" value="Clients" {{if containsCategory .AllowedCategories "Clients"}}checked{{end}}> Clients</label>
                        <label class="category-checkbox"><input type="checkbox" name="categories" value="Finance" {{if containsCategory .AllowedCategories "Finance"}}checked{{end}}> Finance</label>
                        <label class="category-checkbox"><input type="checkbox" name="categories" value="Auth" {{if containsCategory .AllowedCategories "Auth"}}checked{{end}}> Auth</label>
                        <label class="category-checkbox"><input type="checkbox" name="categories" value="Marketing" {{if containsCategory .AllowedCategories "Marketing"}}checked{{end}}> Marketing</label>
                        <label class="category-checkbox"><input type="checkbox" name="categories" value="Product Updates" {{if containsCategory .AllowedCategories "Product Updates"}}checked{{end}}> Product Updates</label>
                        <label class="category-checkbox"><input type="checkbox" name="categories" value="Ops" {{if containsCategory .AllowedCategories "Ops"}}checked{{end}}> Ops</label>
                        <label class="category-checkbox"><input type="checkbox" name="categories" value="Community" {{if containsCategory .AllowedCategories "Community"}}checked{{end}}> Community</label>
                        <label class="category-checkbox"><input type="checkbox" name="categories" value="Projects" {{if containsCategory .AllowedCategories "Projects"}}checked{{end}}> Projects</label>
                        <label class="category-checkbox"><input type="checkbox" name="categories" value="Later" {{if containsCategory .AllowedCategories "Later"}}checked{{end}}> Later</label>
                    </div>
                    <div>
                        <label><input type="checkbox" name="auto_archive" value="1" {{if .AutoArchive}}checked{{end}}> Auto Archive</label>
                    </div>
                    <div style="margin-top: 10px;">
                        <button type="submit" class="btn btn-save">Save</button>
                        <button type="button" onclick="toggleEdit({{.ID}})" class="btn">Cancel</button>
                    </div>
                </form>
            </div>
        </div>
        {{end}}
    </div>

    <script>
    function toggleEdit(id) {
        const viewEl = document.getElementById('view-' + id);
        const editEl = document.getElementById('edit-' + id);
        if (editEl.classList.contains('active')) {
            editEl.classList.remove('active');
            viewEl.style.display = 'block';
        } else {
            editEl.classList.add('active');
            viewEl.style.display = 'none';
        }
    }
    </script>
</body>
</html>
`

const categoriesTemplate = `
<!DOCTYPE html>
<html>
<head>
    <title>Category Management - Automatr</title>
    <style>
        body {
            font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
            max-width: 1200px;
            margin: 0 auto;
            padding: 20px;
            background: #f5f5f5;
        }
        nav {
            background: white;
            padding: 15px;
            border-radius: 8px;
            margin-bottom: 20px;
            box-shadow: 0 2px 4px rgba(0,0,0,0.1);
        }
        nav a {
            margin-right: 20px;
            text-decoration: none;
            color: #007bff;
        }
        nav a:hover { text-decoration: underline; }
        h1 { color: #333; }
        .category-card {
            background: white;
            padding: 20px;
            margin-bottom: 15px;
            border-radius: 8px;
            box-shadow: 0 2px 4px rgba(0,0,0,0.1);
        }
        .category-header {
            display: flex;
            justify-content: space-between;
            align-items: center;
            margin-bottom: 15px;
        }
        .category-name {
            font-weight: 600;
            font-size: 18px;
            color: #333;
        }
        .badge {
            display: inline-block;
            padding: 4px 12px;
            border-radius: 12px;
            font-size: 12px;
            font-weight: 600;
            margin-left: 10px;
        }
        .badge-inbox { background: #ffc107; color: #000; }
        .badge-archive { background: #28a745; color: white; }
        .badge-urgent { background: #dc3545; color: white; }
        .badge-high { background: #fd7e14; color: white; }
        .badge-medium { background: #17a2b8; color: white; }
        .badge-low { background: #6c757d; color: white; }
        .category-description {
            color: #666;
            margin-bottom: 10px;
            font-size: 14px;
        }
        .summarization-prompt {
            background: #f8f9fa;
            padding: 10px;
            border-left: 4px solid #007bff;
            margin-top: 10px;
            font-size: 13px;
            font-family: monospace;
            white-space: pre-wrap;
        }
        .btn {
            padding: 6px 12px;
            border: none;
            border-radius: 4px;
            cursor: pointer;
            font-size: 13px;
            margin-right: 5px;
        }
        .btn-edit { background: #007bff; color: white; }
        .btn-save { background: #28a745; color: white; }
        .btn-cancel { background: #6c757d; color: white; }
        .btn-delete { background: #dc3545; color: white; }
        .edit-form { display: none; }
        .edit-form.active { display: block; }
        .form-group {
            margin-bottom: 15px;
        }
        .form-group label {
            display: block;
            font-weight: 600;
            margin-bottom: 5px;
            font-size: 14px;
        }
        .form-group input[type="text"],
        .form-group textarea,
        .form-group select {
            width: 100%;
            padding: 8px;
            font-size: 14px;
            border: 1px solid #ddd;
            border-radius: 4px;
            box-sizing: border-box;
        }
        .form-group textarea {
            resize: vertical;
            min-height: 60px;
            font-family: monospace;
        }
        .form-group input[type="checkbox"] {
            margin-right: 5px;
        }
        .add-category-form {
            background: white;
            padding: 20px;
            margin-bottom: 20px;
            border-radius: 8px;
            box-shadow: 0 2px 4px rgba(0,0,0,0.1);
        }
        .toggle-add {
            background: #007bff;
            color: white;
            padding: 10px 20px;
            border: none;
            border-radius: 4px;
            cursor: pointer;
            font-size: 14px;
            margin-bottom: 20px;
        }
    </style>
</head>
<body>
    <nav>
        <a href="/review">📧 Review</a>
        <a href="/whitelists">✅ Whitelists</a>
        <a href="/learned-rules">📚 Learned Rules</a>
        <a href="/categories">🏷️ Categories</a>
        <a href="/stats">📊 Stats</a>
        <a href="/">🏠 Home</a>
    </nav>

    <h1>🏷️ Category Management</h1>

    <button onclick="toggleAddForm()" class="toggle-add">+ Add New Category</button>

    <div id="add-category-form" class="add-category-form" style="display: none;">
        <h3>Add New Category</h3>
        <form method="POST" action="/category/add">
            <div class="form-group">
                <label>Category Name *</label>
                <input type="text" name="name" required placeholder="e.g., Work Projects">
            </div>
            <div class="form-group">
                <label>Description</label>
                <input type="text" name="description" placeholder="Brief description of this category">
            </div>
            <div class="form-group">
                <label>Summarization Prompt</label>
                <textarea name="summarization_prompt" placeholder="How should emails in this category be summarized?"></textarea>
            </div>
            <div class="form-group">
                <label>Default Priority</label>
                <select name="priority_default">
                    <option value="urgent">Urgent</option>
                    <option value="high">High</option>
                    <option value="medium" selected>Medium</option>
                    <option value="low">Low</option>
                    <option value="spam">Spam</option>
                </select>
            </div>
            <div class="form-group">
                <label>
                    <input type="checkbox" name="keep_in_inbox" value="1">
                    Keep in Inbox (don't auto-archive)
                </label>
            </div>
            <button type="submit" class="btn btn-save">Add Category</button>
            <button type="button" onclick="toggleAddForm()" class="btn btn-cancel">Cancel</button>
        </form>
    </div>

    {{range .Categories}}
    <div class="category-card">
        <div class="category-header">
            <div>
                <span class="category-name">{{.Name}}</span>
                {{if .KeepInInbox}}
                    <span class="badge badge-inbox">Keep in Inbox</span>
                {{else}}
                    <span class="badge badge-archive">Can Archive</span>
                {{end}}
                {{if eq .PriorityDefault "urgent"}}
                    <span class="badge badge-urgent">Urgent</span>
                {{else if eq .PriorityDefault "high"}}
                    <span class="badge badge-high">High</span>
                {{else if eq .PriorityDefault "medium"}}
                    <span class="badge badge-medium">Medium</span>
                {{else if eq .PriorityDefault "low"}}
                    <span class="badge badge-low">Low</span>
                {{end}}
            </div>
            <div>
                <button onclick="toggleEdit({{.ID}})" class="btn btn-edit">✎ Edit</button>
                <form method="POST" action="/category/delete" style="display:inline">
                    <input type="hidden" name="id" value="{{.ID}}">
                    <button type="submit" class="btn btn-delete" onclick="return confirm('Delete this category? This may affect existing classifications.')">Delete</button>
                </form>
            </div>
        </div>

        <div id="view-{{.ID}}">
            {{if .Description}}
            <div class="category-description">{{.Description}}</div>
            {{end}}
            {{if .SummarizationPrompt}}
            <div class="summarization-prompt">📝 Summarization: {{.SummarizationPrompt}}</div>
            {{end}}
        </div>

        <div id="edit-{{.ID}}" class="edit-form">
            <form method="POST" action="/category/update">
                <input type="hidden" name="id" value="{{.ID}}">
                <div class="form-group">
                    <label>Description</label>
                    <input type="text" name="description" value="{{.Description}}">
                </div>
                <div class="form-group">
                    <label>Summarization Prompt</label>
                    <textarea name="summarization_prompt">{{.SummarizationPrompt}}</textarea>
                </div>
                <div class="form-group">
                    <label>Default Priority</label>
                    <select name="priority_default">
                        <option value="urgent" {{if eq .PriorityDefault "urgent"}}selected{{end}}>Urgent</option>
                        <option value="high" {{if eq .PriorityDefault "high"}}selected{{end}}>High</option>
                        <option value="medium" {{if eq .PriorityDefault "medium"}}selected{{end}}>Medium</option>
                        <option value="low" {{if eq .PriorityDefault "low"}}selected{{end}}>Low</option>
                        <option value="spam" {{if eq .PriorityDefault "spam"}}selected{{end}}>Spam</option>
                    </select>
                </div>
                <div class="form-group">
                    <label>
                        <input type="checkbox" name="keep_in_inbox" value="1" {{if .KeepInInbox}}checked{{end}}>
                        Keep in Inbox (don't auto-archive)
                    </label>
                </div>
                <button type="submit" class="btn btn-save">Save</button>
                <button type="button" onclick="toggleEdit({{.ID}})" class="btn btn-cancel">Cancel</button>
            </form>
        </div>
    </div>
    {{end}}

    <script>
    function toggleEdit(id) {
        const viewEl = document.getElementById('view-' + id);
        const editEl = document.getElementById('edit-' + id);
        if (editEl.classList.contains('active')) {
            editEl.classList.remove('active');
            viewEl.style.display = 'block';
        } else {
            editEl.classList.add('active');
            viewEl.style.display = 'none';
        }
    }

    function toggleAddForm() {
        const form = document.getElementById('add-category-form');
        form.style.display = form.style.display === 'none' ? 'block' : 'none';
    }
    </script>
</body>
</html>
`

const digestUITemplate = `
<!DOCTYPE html>
<html>
<head>
    <title>Create Digest - Automatr</title>
    <style>
        body {
            font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
            max-width: 800px;
            margin: 50px auto;
            padding: 20px;
            background: #f5f5f5;
        }
        nav {
            background: white;
            padding: 15px;
            border-radius: 8px;
            margin-bottom: 20px;
            box-shadow: 0 2px 4px rgba(0,0,0,0.1);
        }
        nav a {
            margin-right: 20px;
            text-decoration: none;
            color: #007bff;
        }
        nav a:hover { text-decoration: underline; }
        h1 { color: #333; }
        .form-card {
            background: white;
            padding: 30px;
            border-radius: 8px;
            box-shadow: 0 2px 4px rgba(0,0,0,0.1);
        }
        .form-group {
            margin-bottom: 20px;
        }
        .form-group label {
            display: block;
            font-weight: 600;
            margin-bottom: 8px;
        }
        .btn {
            padding: 12px 24px;
            border: none;
            border-radius: 4px;
            cursor: pointer;
            font-size: 16px;
            font-weight: 600;
        }
        .btn-primary { background: #007bff; color: white; }
        .btn-primary:hover { background: #0056b3; }
        .option {
            background: #f8f9fa;
            padding: 15px;
            margin-bottom: 10px;
            border-radius: 4px;
            cursor: pointer;
            border: 2px solid transparent;
        }
        .option:hover { border-color: #007bff; }
        .option input[type="radio"] {
            margin-right: 10px;
        }
        .option-label {
            font-weight: 600;
            font-size: 16px;
        }
        .option-desc {
            color: #666;
            font-size: 14px;
            margin-top: 5px;
        }
    </style>
</head>
<body>
    <nav>
        <a href="/review">📧 Review</a>
        <a href="/whitelists">✅ Whitelists</a>
        <a href="/learned-rules">📚 Learned Rules</a>
        <a href="/categories">🏷️ Categories</a>
        <a href="/digest">📊 Create Digest</a>
        <a href="/">🏠 Home</a>
    </nav>

    <h1>📊 Create Email Digest</h1>

    <div class="form-card">
        <form method="POST" action="/digest/create">
            <div class="form-group">
                <label>Select Time Period:</label>
                
                <div class="option">
                    <input type="radio" name="timespan" value="auto" id="auto" checked>
                    <label for="auto" class="option-label">Auto (Since Last Digest)</label>
                    <div class="option-desc">Creates digest from the end of the last digest to now (or last 24h if no previous digest)</div>
                </div>

                <div class="option">
                    <input type="radio" name="timespan" value="4h" id="4h">
                    <label for="4h" class="option-label">Last 4 Hours</label>
                </div>

                <div class="option">
                    <input type="radio" name="timespan" value="12h" id="12h">
                    <label for="12h" class="option-label">Last 12 Hours</label>
                </div>

                <div class="option">
                    <input type="radio" name="timespan" value="24h" id="24h">
                    <label for="24h" class="option-label">Last 24 Hours</label>
                </div>

                <div class="option">
                    <input type="radio" name="timespan" value="48h" id="48h">
                    <label for="48h" class="option-label">Last 48 Hours</label>
                </div>

                <div class="option">
                    <input type="radio" name="timespan" value="168h" id="week">
                    <label for="week" class="option-label">Last Week</label>
                </div>
            </div>

            <button type="submit" class="btn btn-primary">Generate Digest</button>
        </form>
    </div>
</body>
</html>
`

const digestViewTemplate = `
<!DOCTYPE html>
<html>
<head>
    <title>Email Digest - Automatr</title>
    <style>
        body {
            font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
            max-width: 1000px;
            margin: 20px auto;
            padding: 20px;
            background: #f5f5f5;
        }
        nav {
            background: white;
            padding: 15px;
            border-radius: 8px;
            margin-bottom: 20px;
            box-shadow: 0 2px 4px rgba(0,0,0,0.1);
        }
        nav a {
            margin-right: 20px;
            text-decoration: none;
            color: #007bff;
        }
        nav a:hover { text-decoration: underline; }
        h1 { color: #333; }
        .meta-summary {
            background: #e3f2fd;
            padding: 20px;
            border-radius: 8px;
            margin-bottom: 30px;
            border-left: 4px solid #2196f3;
        }
        .actions {
            margin-bottom: 30px;
        }
        .btn {
            padding: 10px 20px;
            border: none;
            border-radius: 4px;
            cursor: pointer;
            text-decoration: none;
            display: inline-block;
            margin-right: 10px;
            font-size: 14px;
        }
        .btn-export { background: #28a745; color: white; }
        .btn-email { background: #007bff; color: white; }
        .category-section {
            background: white;
            padding: 20px;
            margin-bottom: 20px;
            border-radius: 8px;
            box-shadow: 0 1px 3px rgba(0,0,0,0.1);
        }
        .category-header {
            font-size: 20px;
            font-weight: 600;
            margin-bottom: 15px;
            color: #333;
            border-bottom: 2px solid #007bff;
            padding-bottom: 10px;
        }
        .email-item {
            padding: 10px 0;
            border-bottom: 1px solid #eee;
        }
        .email-item:last-child { border-bottom: none; }
        .email-summary {
            color: #333;
            margin-bottom: 5px;
        }
        .email-link {
            color: #007bff;
            text-decoration: none;
            font-size: 13px;
        }
        .email-link:hover { text-decoration: underline; }
        .period {
            color: #666;
            font-size: 14px;
            margin-bottom: 20px;
        }
    </style>
</head>
<body>
    <nav>
        <a href="/review">📧 Review</a>
        <a href="/digest">📊 Create Digest</a>
        <a href="/">🏠 Home</a>
    </nav>

    <h1>📊 Email Digest</h1>

    <div class="period">
        <strong>Period:</strong> {{.Digest.StartTime.Format "2006-01-02 15:04"}} to {{.Digest.EndTime.Format "2006-01-02 15:04"}}
    </div>

    {{if .Digest.MetaSummary}}
    <div class="meta-summary">
        {{.Digest.MetaSummary}}
    </div>
    {{end}}

    <div class="actions">
        <a href="/digest/export?id={{.Digest.ID}}" class="btn btn-export">📥 Export to Markdown</a>
    </div>

    {{range .CategoryOrder}}
        {{$category := .}}
        {{$emails := index $.Inbox $category}}
        {{if $emails}}
        <div class="category-section">
            <div class="category-header">{{$category}} ({{len $emails}})</div>
            {{range $emails}}
            <div class="email-item">
                <div class="email-summary">{{.Summary}}</div>
                <a href="{{.WebLink}}" class="email-link" target="_blank">↗ Open in Outlook</a>
            </div>
            {{end}}
        </div>
        {{end}}
    {{end}}

    {{if .FiledAway}}
    <div class="category-section">
        <div class="category-header">Filed away ({{len .FiledAway}})</div>
        {{range .FiledAway}}
        <div class="email-item">
            <div class="email-summary">{{.Category}} · {{.From}} — {{.Subject}}</div>
            <a href="{{.WebLink}}" class="email-link" target="_blank">↗ Open in Outlook</a>
        </div>
        {{end}}
    </div>
    {{end}}
</body>
</html>
`
