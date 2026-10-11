package daemon

import (
	"context"
	"database/sql"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"time"

	"github.com/clauswitt/automatr/internal/archive"
	"github.com/clauswitt/automatr/internal/classify"
	"github.com/clauswitt/automatr/internal/graph"
)

// Server handles feedback HTTP requests
type Server struct {
	db           *sql.DB
	port         int
	autoStop     bool
	stopAfter    time.Duration
	lastAccess   time.Time
	server       *http.Server
	reviewServer *ReviewServer
}

type FeedbackRequest struct {
	MessageID string
	Action    string // "urgent", "spam", "correct", "wrong"
	Category  string // optional custom category
	Priority  string // optional custom priority
	Reason    string // optional reason
}

// NewServer creates a new daemon server
func NewServer(db *sql.DB, port int, autoStop bool) *Server {
	return &Server{
		db:         db,
		port:       port,
		autoStop:   autoStop,
		stopAfter:  24 * time.Hour,
		lastAccess: time.Now(),
	}
}

// SetReviewServer sets the review server for UI functionality
func (s *Server) SetReviewServer(client *graph.Client, classifier *classify.Classifier, archiver *archive.Archiver) {
	s.reviewServer = NewReviewServer(s.db, client, classifier)
	s.reviewServer.archiver = archiver
}

// Start begins the HTTP server
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()

	// Feedback endpoints
	mux.HandleFunc("/urgent", s.handleUrgent)
	mux.HandleFunc("/spam", s.handleSpam)
	mux.HandleFunc("/correct", s.handleCorrect)
	mux.HandleFunc("/wrong", s.handleWrong)
	mux.HandleFunc("/feedback", s.handleCustomFeedback)

	// Review UI endpoints
	if s.reviewServer != nil {
		mux.HandleFunc("/review", s.reviewServer.HandleReviewUI)
		mux.HandleFunc("/reclassify", s.reviewServer.HandleReclassify)
		mux.HandleFunc("/whitelists", s.reviewServer.HandleWhitelistsUI)
		mux.HandleFunc("/whitelist/add", s.reviewServer.HandleWhitelistAdd)
		mux.HandleFunc("/whitelist/update", s.reviewServer.HandleWhitelistUpdate)
		mux.HandleFunc("/whitelist/remove", s.reviewServer.HandleWhitelistRemove)
		mux.HandleFunc("/learned-rules", s.reviewServer.HandleLearnedRulesUI)
		mux.HandleFunc("/learned-rule/update", s.reviewServer.HandleLearnedRuleUpdate)
		mux.HandleFunc("/learned-rule/delete", s.reviewServer.HandleLearnedRuleDelete)
		mux.HandleFunc("/categories", s.reviewServer.HandleCategoriesUI)
		mux.HandleFunc("/category/add", s.reviewServer.HandleCategoryAdd)
		mux.HandleFunc("/category/update", s.reviewServer.HandleCategoryUpdate)
		mux.HandleFunc("/category/delete", s.reviewServer.HandleCategoryDelete)
		mux.HandleFunc("/digest", s.reviewServer.HandleDigestUI)
		mux.HandleFunc("/digest/create", s.reviewServer.HandleCreateDigest)
		mux.HandleFunc("/digest/view", s.reviewServer.HandleViewDigest)
		mux.HandleFunc("/digest/export", s.reviewServer.HandleExportDigestMarkdown)
	} else {
		mux.HandleFunc("/review", s.handleReviewPlaceholder)
	}

	// Other UI endpoints
	mux.HandleFunc("/stats", s.handleStats)
	mux.HandleFunc("/", s.handleIndex)

	s.server = &http.Server{
		Addr:    fmt.Sprintf(":%d", s.port),
		Handler: mux,
	}

	// Auto-stop goroutine if enabled
	if s.autoStop {
		go s.autoStopMonitor(ctx)
	}

	// Start email processing scheduler if reviewServer is configured
	if s.reviewServer != nil {
		go s.reviewServer.StartScheduler(ctx)
	}

	log.Printf("Daemon server listening on http://localhost:%d", s.port)
	return s.server.ListenAndServe()
}

// Stop gracefully stops the server
func (s *Server) Stop(ctx context.Context) error {
	if s.server != nil {
		return s.server.Shutdown(ctx)
	}
	return nil
}

// Quick action handlers
func (s *Server) handleUrgent(w http.ResponseWriter, r *http.Request) {
	s.updateLastAccess()
	msgID := r.URL.Query().Get("msg")
	if msgID == "" {
		http.Error(w, "Missing msg parameter", http.StatusBadRequest)
		return
	}

	if err := s.recordFeedback(r.Context(), FeedbackRequest{
		MessageID: msgID,
		Action:    "urgent",
		Priority:  "urgent",
		Reason:    "User marked as urgent",
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.renderSuccess(w, "Marked as Urgent", "This email will be treated as urgent priority in future classifications.")
}

func (s *Server) handleSpam(w http.ResponseWriter, r *http.Request) {
	s.updateLastAccess()
	msgID := r.URL.Query().Get("msg")
	if msgID == "" {
		http.Error(w, "Missing msg parameter", http.StatusBadRequest)
		return
	}

	if err := s.recordFeedback(r.Context(), FeedbackRequest{
		MessageID: msgID,
		Action:    "spam",
		Category:  "Marketing",
		Priority:  "spam",
		Reason:    "User marked as spam",
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.renderSuccess(w, "Marked as Spam", "Similar emails will be filtered as spam in the future.")
}

func (s *Server) handleCorrect(w http.ResponseWriter, r *http.Request) {
	s.updateLastAccess()
	msgID := r.URL.Query().Get("msg")
	page := r.URL.Query().Get("page")
	if msgID == "" {
		http.Error(w, "Missing msg parameter", http.StatusBadRequest)
		return
	}

	if err := s.recordFeedback(r.Context(), FeedbackRequest{
		MessageID: msgID,
		Action:    "correct",
		Reason:    "User confirmed classification",
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.renderSuccessWithBack(w, "Confirmed", "Classification confirmed. This helps improve accuracy!", "/review?page="+page)
}

func (s *Server) handleWrong(w http.ResponseWriter, r *http.Request) {
	s.updateLastAccess()
	msgID := r.URL.Query().Get("msg")
	page := r.URL.Query().Get("page")
	if msgID == "" {
		http.Error(w, "Missing msg parameter", http.StatusBadRequest)
		return
	}

	// Get email details from database
	var subject, fromAddr string
	err := s.db.QueryRow(`SELECT subject, from_addr FROM messages WHERE id = ?`, msgID).Scan(&subject, &fromAddr)
	if err != nil {
		http.Error(w, "Message not found", http.StatusNotFound)
		return
	}

	data := struct {
		MsgID   string
		Subject string
		From    string
		Page    string
	}{
		MsgID:   msgID,
		Subject: subject,
		From:    fromAddr,
		Page:    page,
	}

	// Show form for custom correction
	tmpl := template.Must(template.New("wrong").Parse(`
<!DOCTYPE html>
<html>
<head>
    <title>Correct Classification</title>
    <style>
        body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; max-width: 700px; margin: 50px auto; padding: 20px; }
        .email-info { background: #f8f9fa; padding: 15px; border-radius: 8px; margin-bottom: 20px; }
        .email-info h3 { margin: 0 0 10px 0; color: #333; }
        .email-from { color: #666; margin-bottom: 5px; }
        .email-subject { font-weight: 600; color: #333; }
        label { display: block; margin: 15px 0 5px; font-weight: 600; }
        select, input { width: 100%; padding: 8px; font-size: 14px; box-sizing: border-box; }
        button { background: #007bff; color: white; border: none; padding: 12px 24px; font-size: 16px; cursor: pointer; margin-top: 20px; }
        button:hover { background: #0056b3; }
        .back-link { display: inline-block; margin-bottom: 15px; color: #007bff; text-decoration: none; }
        .back-link:hover { text-decoration: underline; }
    </style>
</head>
<body>
    <a href="/review?page={{.Page}}" class="back-link">← Back to Review</a>

    <div class="email-info">
        <h3>Email Details</h3>
        <div class="email-from">From: {{.From}}</div>
        <div class="email-subject">Subject: {{.Subject}}</div>
    </div>

    <h2>Correct Classification</h2>
    <form action="/feedback" method="GET">
        <input type="hidden" name="msg" value="{{.MsgID}}">
        <input type="hidden" name="page" value="{{.Page}}">

        <label>Category:</label>
        <select name="category">
            <option value="People">People</option>
            <option value="Clients">Clients / Leads</option>
            <option value="Finance">Finance / Receipts</option>
            <option value="Auth">Auth / Security</option>
            <option value="Marketing">Marketing / Newsletters</option>
            <option value="Product Updates">Product Updates</option>
            <option value="Ops">Ops / Alerts</option>
            <option value="Community">Community / Social</option>
            <option value="Projects">Projects</option>
            <option value="Later">Later / Low Priority</option>
        </select>

        <label>Priority:</label>
        <select name="priority">
            <option value="urgent">Urgent</option>
            <option value="high">High</option>
            <option value="medium" selected>Medium</option>
            <option value="low">Low</option>
            <option value="spam">Spam</option>
        </select>

        <label>Reason (optional):</label>
        <input type="text" name="reason" placeholder="Why this classification?">

        <button type="submit">Submit Correction</button>
    </form>
</body>
</html>
	`))

	tmpl.Execute(w, data)
}

func (s *Server) handleCustomFeedback(w http.ResponseWriter, r *http.Request) {
	s.updateLastAccess()
	msgID := r.URL.Query().Get("msg")
	category := r.URL.Query().Get("category")
	priority := r.URL.Query().Get("priority")
	reason := r.URL.Query().Get("reason")
	page := r.URL.Query().Get("page")

	if msgID == "" {
		http.Error(w, "Missing msg parameter", http.StatusBadRequest)
		return
	}

	if err := s.recordFeedback(r.Context(), FeedbackRequest{
		MessageID: msgID,
		Action:    "custom",
		Category:  category,
		Priority:  priority,
		Reason:    reason,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.renderSuccessWithBack(w, "Correction Recorded",
		fmt.Sprintf("Email reclassified as %s (%s). Future emails will be classified accordingly.", category, priority),
		"/review?page="+page)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	s.updateLastAccess()
	tmpl := template.Must(template.New("index").Parse(`
<!DOCTYPE html>
<html>
<head>
    <title>Automatr Daemon</title>
    <style>
        body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; max-width: 800px; margin: 50px auto; padding: 20px; }
        h1 { color: #333; }
        .status { background: #d4edda; color: #155724; padding: 15px; border-radius: 5px; margin: 20px 0; }
        a { color: #007bff; text-decoration: none; }
        a:hover { text-decoration: underline; }
    </style>
</head>
<body>
    <h1>🤖 Automatr Feedback Daemon</h1>
    <div class="status">
        ✓ Server running on port {{.Port}}<br>
        {{if .AutoStop}}⏱️ Auto-stop enabled (24h inactivity){{else}}♾️ Manual mode{{end}}
    </div>
    <h3>Available Actions:</h3>
    <ul>
        <li><a href="/review">Review Recent Classifications</a></li>
        <li><a href="/digest">📊 Create Email Digest</a></li>
        <li><a href="/whitelists">✅ Manage Whitelists</a></li>
        <li><a href="/learned-rules">📚 View Learned Rules</a></li>
        <li><a href="/categories">🏷️ Manage Categories</a></li>
        <li><a href="/stats">View Statistics</a></li>
    </ul>
</body>
</html>
	`))

	data := struct {
		Port     int
		AutoStop bool
	}{
		Port:     s.port,
		AutoStop: s.autoStop,
	}

	tmpl.Execute(w, data)
}

func (s *Server) handleReviewPlaceholder(w http.ResponseWriter, r *http.Request) {
	s.updateLastAccess()
	fmt.Fprintf(w, "<h1>Review Interface</h1><p>Review server not initialized. Restart daemon with full context.</p>")
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	s.updateLastAccess()
	fmt.Fprintf(w, "<h1>Statistics</h1><p>Coming soon...</p>")
}

func (s *Server) recordFeedback(ctx context.Context, req FeedbackRequest) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO user_feedback (message_id, action, category, priority, reason, created_at)
		VALUES (?, ?, ?, ?, ?, datetime('now'))
	`, req.MessageID, req.Action, req.Category, req.Priority, req.Reason)

	if err != nil {
		return fmt.Errorf("failed to record feedback: %w", err)
	}

	// Note: Feedback will be processed in batch later via ProcessFeedback()
	// This keeps the feedback endpoint fast and async
	return nil
}

func (s *Server) renderSuccess(w http.ResponseWriter, title, message string) {
	tmpl := template.Must(template.New("success").Parse(`
<!DOCTYPE html>
<html>
<head>
    <title>{{.Title}}</title>
    <style>
        body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; max-width: 600px; margin: 50px auto; padding: 20px; text-align: center; }
        .success { background: #d4edda; color: #155724; padding: 30px; border-radius: 10px; margin: 20px 0; }
        h2 { color: #155724; margin: 0 0 15px 0; }
        p { font-size: 16px; }
        a { color: #007bff; text-decoration: none; }
    </style>
</head>
<body>
    <div class="success">
        <h2>✓ {{.Title}}</h2>
        <p>{{.Message}}</p>
    </div>
    <p><a href="/">← Back to Dashboard</a></p>
</body>
</html>
	`))

	data := struct {
		Title   string
		Message string
	}{
		Title:   title,
		Message: message,
	}

	tmpl.Execute(w, data)
}

func (s *Server) renderSuccessWithBack(w http.ResponseWriter, title, message, backURL string) {
	tmpl := template.Must(template.New("success").Parse(`
<!DOCTYPE html>
<html>
<head>
    <title>{{.Title}}</title>
    <style>
        body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; max-width: 600px; margin: 50px auto; padding: 20px; text-align: center; }
        .success { background: #d4edda; color: #155724; padding: 30px; border-radius: 10px; margin: 20px 0; }
        h2 { color: #155724; margin: 0 0 15px 0; }
        p { font-size: 16px; }
        a { color: #007bff; text-decoration: none; }
    </style>
</head>
<body>
    <div class="success">
        <h2>✓ {{.Title}}</h2>
        <p>{{.Message}}</p>
    </div>
    <p><a href="{{.BackURL}}">← Back to Review</a></p>
</body>
</html>
	`))

	data := struct {
		Title   string
		Message string
		BackURL string
	}{
		Title:   title,
		Message: message,
		BackURL: backURL,
	}

	tmpl.Execute(w, data)
}

func (s *Server) updateLastAccess() {
	s.lastAccess = time.Now()
}

func (s *Server) autoStopMonitor(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if time.Since(s.lastAccess) > s.stopAfter {
				log.Println("Auto-stopping daemon due to inactivity")
				s.Stop(ctx)
				return
			}
		}
	}
}
