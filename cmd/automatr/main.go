package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/clauswitt/automatr/internal/archive"
	"github.com/clauswitt/automatr/internal/auth"
	"github.com/clauswitt/automatr/internal/classify"
	"github.com/clauswitt/automatr/internal/daemon"
	"github.com/clauswitt/automatr/internal/graph"
	"github.com/clauswitt/automatr/internal/store"
	"github.com/clauswitt/automatr/internal/summarize"
	"github.com/clauswitt/automatr/internal/todoist"
	"gopkg.in/yaml.v3"
)

const version = "0.1.0"

func main() {
	var (
		configPath      = flag.String("config", os.ExpandEnv("$HOME/.automatr/config.yaml"), "Path to config file")
		showVersion     = flag.Bool("version", false, "Show version")
		authOnly        = flag.Bool("auth", false, "Run authentication flow only")
		listCmd         = flag.Bool("list", false, "List recent emails")
		unreadCmd       = flag.Bool("unread", false, "List unread emails only")
		foldersCmd      = flag.Bool("folders", false, "List all mail folders")
		searchCmd       = flag.String("search", "", "Search emails by subject/sender")
		readCmd         = flag.String("read", "", "Read email by ID")
		classifyCmd     = flag.String("classify", "", "Classify email by ID")
		initEmbedCmd    = flag.Bool("init-embeddings", false, "Initialize category embeddings")
		reviewCmd       = flag.Bool("review", false, "Batch review and classify recent emails")
		reviewTimespan  = flag.String("review-timespan", "48h", "Timespan to review (e.g., 24h, 48h, 7d, 2w)")
		processFeedback = flag.Bool("process-feedback", false, "Process pending user feedback to improve classification")
		daemonCmd       = flag.Bool("daemon", false, "Start feedback daemon server")
		daemonPort      = flag.Int("daemon-port", 8383, "Daemon server port")
		daemonAutoStop  = flag.Bool("daemon-auto-stop", false, "Auto-stop daemon after 24h inactivity")
		daemonBg        = flag.Bool("daemon-background", false, "Run daemon in background")
		digestCmd       = flag.Bool("digest", false, "Create email digest in markdown format")
		digestTimespan  = flag.String("digest-timespan", "auto", "Digest timespan (auto, 4h, 12h, 24h, 48h, 7d, 14d)")
		digestOutput    = flag.String("digest-output", "", "Output file for digest (stdout if empty)")
		migrateLinks    = flag.Bool("migrate-links", false, "Migrate existing messages to use stable Message-ID links")
		syncCmd         = flag.Bool("sync", false, "Sync emails: fetch, classify, archive, and create digest (one-shot)")
		syncTimespan    = flag.String("sync-timespan", "48h", "Timespan to sync (e.g., 24h, 48h, 7d, 2w, or all for the whole inbox)")
		syncOutput      = flag.String("sync-output", "", "Output file for digest (stdout if empty)")
		syncNoSummary   = flag.Bool("sync-no-summary", false, "Skip AI summary generation (faster, just show subject lines)")
		dryRunLog       = flag.String("dry-run-log", "", "If set, do NOT archive — append [WOULD-ARCHIVE] lines to this file instead")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("automatr v%s\n", version)
		return
	}

	ctx := context.Background()

	// Load config
	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Initialize auth
	authMgr := auth.NewManager(cfg.ClientID, cfg.ClientSecret, cfg.TenantID, cfg.RedirectURI, cfg.Scopes)
	token, err := authMgr.GetToken(ctx)
	if err != nil {
		log.Fatalf("Authentication failed: %v", err)
	}

	if *authOnly {
		fmt.Println("✓ Authentication successful")
		return
	}

	// Initialize Graph client
	client := graph.NewClient(token.AccessToken)

	// Initialize database
	db, err := store.New(cfg.DBPath)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer db.Close()

	// Handle commands
	switch {
	case *syncCmd:
		if err := syncEmails(ctx, cfg, client, db, *syncTimespan, *syncOutput, *syncNoSummary, *dryRunLog); err != nil {
			log.Fatalf("Failed to sync emails: %v", err)
		}
	case *migrateLinks:
		if err := migrateToStableLinks(ctx, client, db); err != nil {
			log.Fatalf("Failed to migrate links: %v", err)
		}
	case *digestCmd:
		if err := createDigest(ctx, cfg, client, db, *digestTimespan, *digestOutput); err != nil {
			log.Fatalf("Failed to create digest: %v", err)
		}
	case *daemonCmd:
		if err := runDaemon(ctx, cfg, client, db, *daemonPort, *daemonAutoStop, *daemonBg); err != nil {
			log.Fatalf("Failed to run daemon: %v", err)
		}
	case *processFeedback:
		if err := processPendingFeedback(ctx, cfg, db); err != nil {
			log.Fatalf("Failed to process feedback: %v", err)
		}
	case *reviewCmd:
		if err := batchReview(ctx, cfg, client, db, *reviewTimespan); err != nil {
			log.Fatalf("Failed to run batch review: %v", err)
		}
	case *foldersCmd:
		if err := listFolders(ctx, client); err != nil {
			log.Fatalf("Failed to list folders: %v", err)
		}
	case *initEmbedCmd:
		if err := initializeEmbeddings(ctx, cfg, db); err != nil {
			log.Fatalf("Failed to initialize embeddings: %v", err)
		}
	case *classifyCmd != "":
		if err := classifyEmail(ctx, cfg, client, db, *classifyCmd); err != nil {
			log.Fatalf("Failed to classify email: %v", err)
		}
	case *searchCmd != "":
		if err := searchEmails(ctx, client, *searchCmd); err != nil {
			log.Fatalf("Failed to search emails: %v", err)
		}
	case *unreadCmd:
		if err := listUnreadEmails(ctx, client); err != nil {
			log.Fatalf("Failed to list unread emails: %v", err)
		}
	case *listCmd:
		if err := listEmails(ctx, client); err != nil {
			log.Fatalf("Failed to list emails: %v", err)
		}
	case *readCmd != "":
		if err := readEmail(ctx, client, *readCmd); err != nil {
			log.Fatalf("Failed to read email: %v", err)
		}
	default:
		fmt.Println("No command specified. Use -h for help.")
	}
}

func listEmails(ctx context.Context, client *graph.Client) error {
	messages, err := client.ListMessages(ctx, 20)
	if err != nil {
		return err
	}

	fmt.Printf("Found %d recent emails (ordered by receivedDateTime DESC):\n\n", len(messages))
	for i, msg := range messages {
		fmt.Printf("%d. [%s] %s\n", i+1, msg.ReceivedDateTime, msg.Subject)
		fmt.Printf("   From: %s\n", msg.From.EmailAddress.Address)
		if msg.IsRead {
			fmt.Printf("   Status: Read\n")
		} else {
			fmt.Printf("   Status: UNREAD\n")
		}
		fmt.Printf("   ID: %s\n\n", msg.ID)
	}

	return nil
}

func searchEmails(ctx context.Context, client *graph.Client, query string) error {
	messages, err := client.SearchMessages(ctx, query)
	if err != nil {
		return err
	}

	fmt.Printf("Found %d emails matching '%s':\n\n", len(messages), query)
	for i, msg := range messages {
		fmt.Printf("%d. [%s] %s\n", i+1, msg.ReceivedDateTime, msg.Subject)
		fmt.Printf("   From: %s <%s>\n", msg.From.EmailAddress.Name, msg.From.EmailAddress.Address)
		if msg.IsRead {
			fmt.Printf("   Status: Read\n")
		} else {
			fmt.Printf("   Status: UNREAD\n")
		}
		fmt.Printf("   ID: %s\n\n", msg.ID)
	}

	return nil
}

func listUnreadEmails(ctx context.Context, client *graph.Client) error {
	messages, err := client.ListUnreadMessages(ctx, 20)
	if err != nil {
		return err
	}

	fmt.Printf("Found %d unread emails (ordered by receivedDateTime DESC):\n\n", len(messages))
	for i, msg := range messages {
		fmt.Printf("%d. [%s] %s\n", i+1, msg.ReceivedDateTime, msg.Subject)
		fmt.Printf("   From: %s\n", msg.From.EmailAddress.Address)
		fmt.Printf("   ID: %s\n\n", msg.ID)
	}

	return nil
}

func listFolders(ctx context.Context, client *graph.Client) error {
	folders, err := client.ListFolders(ctx)
	if err != nil {
		return err
	}

	fmt.Printf("Found %d mail folders:\n\n", len(folders))
	for i, folder := range folders {
		fmt.Printf("%d. %s\n", i+1, folder.DisplayName)
		fmt.Printf("   Total: %d | Unread: %d\n", folder.TotalItemCount, folder.UnreadItemCount)
		fmt.Printf("   ID: %s\n\n", folder.ID)
	}

	return nil
}

func readEmail(ctx context.Context, client *graph.Client, messageID string) error {
	msg, err := client.GetMessage(ctx, messageID)
	if err != nil {
		return err
	}

	fmt.Printf("Subject: %s\n", msg.Subject)
	fmt.Printf("From: %s <%s>\n", msg.From.EmailAddress.Name, msg.From.EmailAddress.Address)
	fmt.Printf("Received: %s\n", msg.ReceivedDateTime)
	fmt.Printf("Importance: %s\n", msg.Importance)
	fmt.Printf("\n--- Body ---\n")
	fmt.Println(msg.Body.Content)

	return nil
}

func initializeEmbeddings(ctx context.Context, cfg *Config, db *store.Store) error {
	fmt.Println("Initializing category embeddings...")

	provider, err := buildEmbeddingsProvider(cfg)
	if err != nil {
		return err
	}

	// Create classifier and initialize
	classifier := newClassifier(cfg, provider, db.DB(), true)
	if err := classifier.Initialize(ctx); err != nil {
		return err
	}

	fmt.Println("✓ Category embeddings initialized and cached")
	return nil
}

func runDaemon(ctx context.Context, cfg *Config, client *graph.Client, db *store.Store, port int, autoStop, background bool) error {
	provider, err := buildLLMProvider(cfg)
	if err != nil {
		return err
	}

	// Create classifier
	classifier := newClassifier(cfg, provider, db.DB(), true)
	if err := classifier.Initialize(ctx); err != nil {
		return fmt.Errorf("failed to initialize classifier: %w", err)
	}

	archiver, err := newArchiver(cfg, client, db.DB(), classifier, "")
	if err != nil {
		return err
	}

	// Create server with full functionality
	srv := daemon.NewServer(db.DB(), port, autoStop)
	srv.SetReviewServer(client, classifier, archiver)

	if background {
		// Run in background
		go func() {
			if err := srv.Start(ctx); err != nil && err != http.ErrServerClosed {
				log.Printf("Daemon error: %v", err)
			}
		}()
		fmt.Printf("Daemon started in background on http://localhost:%d\n", port)
		if autoStop {
			fmt.Println("Auto-stop enabled: will shut down after 24h of inactivity")
		}
		fmt.Println("Press Ctrl+C to stop")
		<-ctx.Done()
		return srv.Stop(context.Background())
	}

	// Run in foreground
	fmt.Printf("Starting daemon on http://localhost:%d\n", port)
	if autoStop {
		fmt.Println("Auto-stop enabled: will shut down after 24h of inactivity")
	}
	fmt.Println("Note: For review UI, run: ./automatr -review")
	fmt.Println("Press Ctrl+C to stop")
	return srv.Start(ctx)
}

func createDigest(ctx context.Context, cfg *Config, client *graph.Client, db *store.Store, timespan, outputFile string) error {
	provider, err := buildLLMProvider(cfg)
	if err != nil {
		return err
	}

	// Create classifier
	classifier := newClassifier(cfg, provider, db.DB(), false)
	if err := classifier.Initialize(ctx); err != nil {
		return fmt.Errorf("failed to initialize classifier: %w", err)
	}

	// Create digest service
	digestSvc := summarize.NewDigestService(db.DB(), classifier, client)

	// Calculate time range
	var startTime time.Time
	endTime := time.Now()

	if timespan == "auto" {
		lastEnd, err := digestSvc.GetLastDigestEndTime(ctx)
		if err != nil {
			return fmt.Errorf("failed to get last digest time: %w", err)
		}
		if lastEnd != nil {
			startTime = *lastEnd
		} else {
			startTime = endTime.Add(-24 * time.Hour)
		}
	} else {
		duration, err := time.ParseDuration(timespan)
		if err != nil {
			return fmt.Errorf("invalid timespan: %w", err)
		}
		startTime = endTime.Add(-duration)
	}

	// Create digest
	digest, err := digestSvc.CreateDigest(ctx, startTime, endTime)
	if err != nil {
		return fmt.Errorf("failed to create digest: %w", err)
	}

	// Generate meta summary
	_, err = digestSvc.GenerateMetaSummary(ctx, digest)
	if err != nil {
		return fmt.Errorf("failed to generate meta summary: %w", err)
	}

	// Get category order
	categoryOrder, err := digestSvc.GetCategoryImportance(ctx)
	if err != nil {
		return fmt.Errorf("failed to get category order: %w", err)
	}

	// Generate markdown
	var markdown strings.Builder
	markdown.WriteString("# Email Digest\n\n")
	markdown.WriteString(fmt.Sprintf("**Period:** %s to %s\n\n",
		startTime.Format("2006-01-02 15:04"),
		endTime.Format("2006-01-02 15:04")))

	if digest.MetaSummary != "" {
		markdown.WriteString(digest.MetaSummary)
		markdown.WriteString("\n\n")
	}

	summarize.WriteMarkdownSections(&markdown, digest, categoryOrder)

	// Output to file or stdout
	output := markdown.String()
	if outputFile != "" {
		if err := os.WriteFile(outputFile, []byte(output), 0644); err != nil {
			return fmt.Errorf("failed to write to file: %w", err)
		}
		fmt.Printf("✓ Digest saved to %s\n", outputFile)
		if err := digestSvc.WriteSummaryFile(ctx, digest, outputFile); err != nil {
			fmt.Printf("⚠️  Failed to write summary companion: %v\n", err)
		} else {
			fmt.Printf("✓ Summary companion saved to %s\n", summarize.SummaryFilePath(outputFile))
		}
	} else {
		fmt.Print(output)
	}

	// Note: Archiving now happens during email processing (daemon auto-archive on arrival)
	// No need to archive during digest creation - emails should already be in final folders

	return nil
}

func migrateToStableLinks(ctx context.Context, client *graph.Client, db *store.Store) error {
	fmt.Println("Migrating messages to use stable Message-ID links...")

	// Get all messages that don't have internet_message_id
	rows, err := db.DB().QueryContext(ctx, `
		SELECT id FROM messages
		WHERE internet_message_id IS NULL OR internet_message_id = ''
		ORDER BY received_at DESC
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	var messageIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		messageIDs = append(messageIDs, id)
	}

	fmt.Printf("Found %d messages to update\n", len(messageIDs))

	updated := 0
	failed := 0

	for i, msgID := range messageIDs {
		if i > 0 && i%10 == 0 {
			fmt.Printf("Progress: %d/%d (updated: %d, failed: %d)\n", i, len(messageIDs), updated, failed)
		}

		// Fetch the message from Graph API to get InternetMessageId
		msg, err := client.GetMessage(ctx, msgID)
		if err != nil {
			fmt.Printf("Failed to fetch message %s: %v\n", msgID, err)
			failed++
			continue
		}

		// Update the database with the InternetMessageId
		_, err = db.DB().ExecContext(ctx, `
			UPDATE messages
			SET internet_message_id = ?
			WHERE id = ?
		`, msg.InternetMessageId, msgID)

		if err != nil {
			fmt.Printf("Failed to update message %s: %v\n", msgID, err)
			failed++
			continue
		}

		updated++
	}

	fmt.Printf("\n✓ Migration complete: %d updated, %d failed\n", updated, failed)
	fmt.Println("✓ Clearing email summaries cache to regenerate with new links...")

	// Clear summaries cache so they regenerate with new stable links
	_, err = db.DB().ExecContext(ctx, `DELETE FROM email_summaries`)
	if err != nil {
		return fmt.Errorf("failed to clear summaries cache: %w", err)
	}

	fmt.Println("✓ Done! New digests will use stable links that work even after archiving.")
	return nil
}

func classifyEmail(ctx context.Context, cfg *Config, client *graph.Client, db *store.Store, messageID string) error {
	// Fetch the email
	msg, err := client.GetMessage(ctx, messageID)
	if err != nil {
		return fmt.Errorf("failed to fetch email: %w", err)
	}

	fmt.Printf("Classifying email:\n")
	fmt.Printf("Subject: %s\n", msg.Subject)
	fmt.Printf("From: %s <%s>\n\n", msg.From.EmailAddress.Name, msg.From.EmailAddress.Address)

	provider, err := buildLLMProvider(cfg)
	if err != nil {
		return err
	}

	// Create classifier (dry-run mode)
	classifier := newClassifier(cfg, provider, db.DB(), true)

	// Prepare email input
	bodyText := msg.Body.Content
	if len(bodyText) > 1000 {
		bodyText = bodyText[:1000]
	}

	emailInput := classify.EmailInput{
		From:         msg.From.EmailAddress.Address,
		Subject:      msg.Subject,
		Body:         bodyText,
		Headers:      make(map[string]string), // Would need to fetch headers from API
		ToRecipients: len(msg.ToRecipients),
	}

	// Classify
	result, err := classifier.Classify(ctx, emailInput)
	if err != nil {
		return fmt.Errorf("classification failed: %w", err)
	}

	// Display results
	fmt.Println("--- Classification Result ---")
	fmt.Printf("Category:   %s\n", result.Category)
	fmt.Printf("Confidence: %.2f\n", result.Confidence)
	fmt.Printf("Method:     %s\n", result.Method)
	fmt.Printf("Reasoning:  %s\n", result.Reasoning)

	if result.Triage != nil {
		fmt.Printf("Needs me:   %.2f\n", result.Triage.NeedsMe)
		fmt.Printf("Time-bound: %.2f\n", result.Triage.TimeBound)
		fmt.Printf("Cold:       %.2f\n", result.Triage.ColdOutreach)
	}

	// Show the archive decision
	archiver, err := newArchiver(cfg, client, db.DB(), classifier, "")
	if err != nil {
		return err
	}
	receivedAt, _ := time.Parse(time.RFC3339, msg.ReceivedDateTime)
	decision := archiver.Policy().Decide(result.Triage, classify.MessageState{
		From:       msg.From.EmailAddress.Address,
		Subject:    msg.Subject,
		IsRead:     msg.IsRead,
		Flagged:    msg.Flag.FlagStatus == "flagged",
		Importance: msg.Importance,
		ReceivedAt: receivedAt,
	}, time.Now())
	if decision.Archive {
		fmt.Printf("\n🗄️  Would be ARCHIVED (%s)\n", decision.Reason)
	} else {
		fmt.Printf("\n📬 Would stay in INBOX (%s)\n", decision.Reason)
	}

	return nil
}

type Config struct {
	ClientID     string        `yaml:"client_id"`
	ClientSecret string        `yaml:"client_secret"`
	TenantID     string        `yaml:"tenant_id"`
	RedirectURI  string        `yaml:"redirect_uri"`
	Scopes       []string      `yaml:"scopes"`
	DBPath       string        `yaml:"db_path"`
	Timezone     string        `yaml:"timezone"`
	AI           AIConfig      `yaml:"ai"`
	Archive      ArchiveConfig `yaml:"archive"`
	Todoist      TodoistConfig `yaml:"todoist"`
}

// ArchiveConfig controls auto-archiving. Decisions come from Jev's triage
// questions; without Jev nothing is archived.
type ArchiveConfig struct {
	Mode           string   `yaml:"mode"`                // "dry-run" (default) logs only; "live" moves messages
	ArchiveBelow   float64  `yaml:"archive_below"`       // archive when P(needs me) is below this
	TimeBoundAbove float64  `yaml:"time_bound_above"`    // treat as OTP/magic link above this
	ColdAbove      float64  `yaml:"cold_outreach_above"` // archive unsolicited pitches above this
	KeepAuthFor    string   `yaml:"keep_auth_for"`       // unread OTPs stay this long, e.g. "1h"
	CompanyDomains []string `yaml:"company_domains"`     // never archive mail from these domains
	DryRunLog      string   `yaml:"dry_run_log"`

	// Explicit sender rules, applied before Jev (flags and high importance still win)
	AlwaysKeep    []classify.SenderRule `yaml:"always_keep"`
	AlwaysArchive []classify.SenderRule `yaml:"always_archive"`
}

type AIConfig struct {
	// Primary provider for LLM adjudication (ollama, openai, or anthropic)
	Provider string `yaml:"provider"`

	// OpenAI configuration
	OpenAI OpenAIConfig `yaml:"openai"`

	// Anthropic configuration
	Anthropic AnthropicConfig `yaml:"anthropic"`

	// Ollama configuration
	Ollama OllamaConfig `yaml:"ollama"`

	// Embeddings provider (ollama or openai)
	EmbeddingsProvider string `yaml:"embeddings_provider"`

	// Jev (TypeSafe) classifies ahead of embeddings/LLM when an API key is set
	Jev JevConfig `yaml:"jev"`
}

type JevConfig struct {
	APIKey        string  `yaml:"api_key"`
	Model         string  `yaml:"model"`          // e.g., "jev-latest" or a pinned "jev-1.13.0"
	MinConfidence float64 `yaml:"min_confidence"` // below this → "Needs Review"
}

type OpenAIConfig struct {
	APIKey string `yaml:"api_key"`
	Model  string `yaml:"model"` // e.g., "gpt-4o-mini"
}

type AnthropicConfig struct {
	APIKey string `yaml:"api_key"`
	Model  string `yaml:"model"` // e.g., "claude-3-5-haiku-20241022"
}

type OllamaConfig struct {
	BaseURL    string `yaml:"base_url"`    // e.g., "http://localhost:11434"
	Model      string `yaml:"model"`       // e.g., "qwen2.5:7b"
	EmbedModel string `yaml:"embed_model"` // e.g., "nomic-embed-text"
}

type TodoistConfig struct {
	Enabled          bool                  `yaml:"enabled"`
	APITokenEnv      string                `yaml:"api_token_env"`
	ProjectID        string                `yaml:"project_id"`
	Labels           []string              `yaml:"labels"`
	Priority         int                   `yaml:"priority"`
	NeedsMeAbove     float64               `yaml:"needs_me_above"`
	DefaultDueIn     string                `yaml:"default_due_in"`
	AlwaysTask       []classify.SenderRule `yaml:"always_task"`
	SkipTimeBound    bool                  `yaml:"skip_time_bound"`
	SkipColdOutreach bool                  `yaml:"skip_cold_outreach"`
}

// buildLLMProvider constructs the primary LLM provider for classification/summarization.
// When the primary is ollama and an OpenAI key is present, the result is wrapped
// in a FallbackProvider so callers transparently survive a local Ollama outage.
func buildLLMProvider(cfg *Config) (classify.LLMProvider, error) {
	switch cfg.AI.Provider {
	case "ollama":
		primary := classify.NewOllamaProvider(cfg.AI.Ollama.BaseURL, cfg.AI.Ollama.Model, cfg.AI.Ollama.EmbedModel)
		if cfg.AI.OpenAI.APIKey != "" {
			secondary := classify.NewOpenAIProvider(cfg.AI.OpenAI.APIKey, cfg.AI.OpenAI.Model)
			return classify.NewFallbackProvider(primary, secondary), nil
		}
		return primary, nil
	case "openai":
		return classify.NewOpenAIProvider(cfg.AI.OpenAI.APIKey, cfg.AI.OpenAI.Model), nil
	case "anthropic":
		return classify.NewAnthropicProvider(cfg.AI.Anthropic.APIKey, cfg.AI.Anthropic.Model), nil
	default:
		return nil, fmt.Errorf("unknown AI provider: %s", cfg.AI.Provider)
	}
}

// newClassifier builds a classifier and enables Jev when it is configured.
func newClassifier(cfg *Config, provider classify.LLMProvider, db *sql.DB, dryRun bool) *classify.Classifier {
	c := classify.NewClassifier(provider, db, dryRun)
	if cfg.AI.Jev.APIKey != "" {
		c.SetJev(classify.NewJevClassifier(cfg.AI.Jev.APIKey, cfg.AI.Jev.Model), cfg.AI.Jev.MinConfidence)
	}
	return c
}

// newArchiver builds the archiver from config. A non-empty dryRunLogPath
// (the -dry-run-log flag) forces dry-run regardless of archive.mode.
func newArchiver(cfg *Config, client *graph.Client, db *sql.DB, classifier *classify.Classifier, dryRunLogPath string) (*archive.Archiver, error) {
	hold, err := time.ParseDuration(cfg.Archive.KeepAuthFor)
	if err != nil {
		return nil, fmt.Errorf("invalid archive.keep_auth_for %q: %w", cfg.Archive.KeepAuthFor, err)
	}
	policy := classify.ArchivePolicy{
		ArchiveBelow:      cfg.Archive.ArchiveBelow,
		TimeBoundAbove:    cfg.Archive.TimeBoundAbove,
		ColdOutreachAbove: cfg.Archive.ColdAbove,
		AuthHold:          hold,
		CompanyDomains:    cfg.Archive.CompanyDomains,
		AlwaysKeep:        cfg.Archive.AlwaysKeep,
		AlwaysArchive:     cfg.Archive.AlwaysArchive,
	}
	live := cfg.Archive.Mode == "live" && dryRunLogPath == ""
	logPath := cfg.Archive.DryRunLog
	if dryRunLogPath != "" {
		logPath = dryRunLogPath
	}
	return archive.New(db, client, classifier, policy, live, logPath), nil
}

func newTodoistClient(cfg *Config) (*todoist.Client, time.Duration, error) {
	due, err := time.ParseDuration(cfg.Todoist.DefaultDueIn)
	if err != nil {
		return nil, 0, fmt.Errorf("invalid todoist.default_due_in %q: %w", cfg.Todoist.DefaultDueIn, err)
	}
	if !cfg.Todoist.Enabled {
		return nil, due, nil
	}
	token := os.Getenv(cfg.Todoist.APITokenEnv)
	if token == "" {
		return nil, due, fmt.Errorf("%s is not set", cfg.Todoist.APITokenEnv)
	}
	return todoist.NewClient(token), due, nil
}

func loadStoredTriage(ctx context.Context, db *sql.DB, id string) *classify.Triage {
	var needsMe, timeBound, coldOutreach sql.NullFloat64
	err := db.QueryRowContext(ctx, `SELECT needs_me, time_bound, cold_outreach FROM messages WHERE id = ?`, id).
		Scan(&needsMe, &timeBound, &coldOutreach)
	if err != nil || !needsMe.Valid || !timeBound.Valid || !coldOutreach.Valid {
		return nil
	}
	return &classify.Triage{NeedsMe: needsMe.Float64, TimeBound: timeBound.Float64, ColdOutreach: coldOutreach.Float64}
}

func upsertTodoistTask(ctx context.Context, cfg *Config, client *todoist.Client, fallbackDue time.Duration, msg graph.Message, category string, triage *classify.Triage) (string, error) {
	if triage == nil {
		return "", nil
	}
	if cfg.Todoist.SkipTimeBound && triage.TimeBound > cfg.Archive.TimeBoundAbove {
		return "", nil
	}
	if cfg.Todoist.SkipColdOutreach && triage.ColdOutreach > cfg.Archive.ColdAbove {
		return "", nil
	}

	ruleMatched := false
	for _, rule := range cfg.Todoist.AlwaysTask {
		if rule.Matches(msg.From.EmailAddress.Address, msg.Subject) {
			ruleMatched = true
			break
		}
	}
	if !ruleMatched && triage.NeedsMe < cfg.Todoist.NeedsMeAbove {
		return "", nil
	}

	receivedAt, _ := time.Parse(time.RFC3339, msg.ReceivedDateTime)
	body := msg.Body.Content
	if len(body) > 4000 {
		body = body[:4000]
	}
	reason := fmt.Sprintf("Automatr classified this as requiring attention (needs_me %.2f, category %s).", triage.NeedsMe, category)
	if ruleMatched {
		reason = fmt.Sprintf("Automatr matched an explicit email-action rule (needs_me %.2f, category %s).", triage.NeedsMe, category)
	}
	return client.UpsertEmailTask(ctx, todoist.EmailTask{
		Subject:    msg.Subject,
		From:       msg.From.EmailAddress.Address,
		ReceivedAt: receivedAt,
		WebLink:    msg.WebLink,
		Body:       body,
		DueDate:    todoist.InferDueDate(time.Now(), msg.Subject, body, fallbackDue),
		Priority:   cfg.Todoist.Priority,
		ProjectID:  cfg.Todoist.ProjectID,
		Labels:     cfg.Todoist.Labels,
		Reason:     reason,
	})
}

// buildEmbeddingsProvider constructs the provider used for generating embeddings.
// Anthropic doesn't support embeddings, so we only accept ollama or openai here.
func buildEmbeddingsProvider(cfg *Config) (classify.LLMProvider, error) {
	switch cfg.AI.EmbeddingsProvider {
	case "ollama":
		primary := classify.NewOllamaProvider(cfg.AI.Ollama.BaseURL, cfg.AI.Ollama.Model, cfg.AI.Ollama.EmbedModel)
		if cfg.AI.OpenAI.APIKey != "" {
			secondary := classify.NewOpenAIProvider(cfg.AI.OpenAI.APIKey, cfg.AI.OpenAI.Model)
			return classify.NewFallbackProvider(primary, secondary), nil
		}
		return primary, nil
	case "openai":
		return classify.NewOpenAIProvider(cfg.AI.OpenAI.APIKey, cfg.AI.OpenAI.Model), nil
	default:
		return nil, fmt.Errorf("unknown embeddings provider: %s", cfg.AI.EmbeddingsProvider)
	}
}

func parseTimespan(timespan string) (time.Duration, error) {
	// Try standard duration format first (24h, 48h, etc.)
	duration, err := time.ParseDuration(timespan)
	if err == nil {
		return duration, nil
	}

	// Support shortcuts: 2d (days), 2w (weeks)
	if len(timespan) >= 2 {
		suffix := timespan[len(timespan)-1:]
		valueStr := timespan[:len(timespan)-1]

		var value int
		if _, err := fmt.Sscanf(valueStr, "%d", &value); err == nil {
			switch suffix {
			case "d":
				return time.Duration(value) * 24 * time.Hour, nil
			case "w":
				return time.Duration(value) * 7 * 24 * time.Hour, nil
			}
		}
	}

	return 0, fmt.Errorf("invalid duration format: %s (use 24h, 48h, 7d, 2w, etc.)", timespan)
}

func loadConfig(path string) (*Config, error) {
	// Expand home directory
	if path[:2] == "~/" {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, path[2:])
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	// Set defaults if not specified
	if cfg.DBPath == "" {
		cfg.DBPath = os.ExpandEnv("$HOME/.automatr/automatr.db")
	}
	if cfg.Timezone == "" {
		cfg.Timezone = "Europe/Copenhagen"
	}

	// AI defaults
	if cfg.AI.Provider == "" {
		cfg.AI.Provider = "openai"
	}
	if cfg.AI.EmbeddingsProvider == "" {
		cfg.AI.EmbeddingsProvider = "openai"
	}
	if cfg.AI.OpenAI.Model == "" {
		cfg.AI.OpenAI.Model = "gpt-4o-mini"
	}
	if cfg.AI.Anthropic.Model == "" {
		cfg.AI.Anthropic.Model = "claude-3-5-haiku-20241022"
	}
	if cfg.AI.Ollama.BaseURL == "" {
		cfg.AI.Ollama.BaseURL = "http://localhost:11434"
	}
	if cfg.AI.Ollama.Model == "" {
		cfg.AI.Ollama.Model = "qwen2.5:7b"
	}
	if cfg.AI.Ollama.EmbedModel == "" {
		cfg.AI.Ollama.EmbedModel = "nomic-embed-text"
	}
	if cfg.AI.Jev.Model == "" {
		cfg.AI.Jev.Model = "jev-latest"
	}
	if cfg.AI.Jev.MinConfidence == 0 {
		cfg.AI.Jev.MinConfidence = 0.6
	}

	// Archive defaults
	if cfg.Archive.Mode == "" {
		cfg.Archive.Mode = "dry-run"
	}
	if cfg.Archive.ArchiveBelow == 0 {
		cfg.Archive.ArchiveBelow = 0.3
	}
	if cfg.Archive.TimeBoundAbove == 0 {
		cfg.Archive.TimeBoundAbove = 0.5
	}
	if cfg.Archive.ColdAbove == 0 {
		cfg.Archive.ColdAbove = 0.85
	}
	if cfg.Archive.KeepAuthFor == "" {
		cfg.Archive.KeepAuthFor = "1h"
	}
	if cfg.Archive.DryRunLog == "" {
		cfg.Archive.DryRunLog = "~/.automatr/would-archive.log"
	}
	if strings.HasPrefix(cfg.Archive.DryRunLog, "~/") {
		home, _ := os.UserHomeDir()
		cfg.Archive.DryRunLog = filepath.Join(home, cfg.Archive.DryRunLog[2:])
	}
	if cfg.Todoist.APITokenEnv == "" {
		cfg.Todoist.APITokenEnv = "TODOIST_API_TOKEN"
	}
	if cfg.Todoist.Priority == 0 {
		cfg.Todoist.Priority = 4
	}
	if cfg.Todoist.NeedsMeAbove == 0 {
		cfg.Todoist.NeedsMeAbove = 0.8
	}
	if cfg.Todoist.DefaultDueIn == "" {
		cfg.Todoist.DefaultDueIn = "48h"
	}
	if len(cfg.Todoist.Labels) == 0 {
		cfg.Todoist.Labels = []string{"@agent-generated", "email-action"}
	}
	if !cfg.Todoist.SkipTimeBound {
		cfg.Todoist.SkipTimeBound = true
	}
	if !cfg.Todoist.SkipColdOutreach {
		cfg.Todoist.SkipColdOutreach = true
	}

	return &cfg, nil
}

func processPendingFeedback(ctx context.Context, cfg *Config, db *store.Store) error {
	fmt.Println("Processing pending user feedback...")

	provider, err := buildLLMProvider(cfg)
	if err != nil {
		return err
	}

	// Create classifier
	classifier := newClassifier(cfg, provider, db.DB(), false)

	// Initialize embeddings
	if err := classifier.Initialize(ctx); err != nil {
		return fmt.Errorf("failed to initialize classifier: %w", err)
	}

	// Process feedback
	count, err := classifier.ProcessFeedback(ctx)
	if err != nil {
		return fmt.Errorf("failed to process feedback: %w", err)
	}

	fmt.Printf("✓ Processed %d feedback items into learned rules/examples\n", count)
	return nil
}

func syncEmails(ctx context.Context, cfg *Config, client *graph.Client, db *store.Store, timespan, outputFile string, noSummary bool, dryRunLogPath string) error {
	// "all" processes the whole inbox regardless of age
	since := time.Unix(0, 0)
	if timespan == "all" {
		fmt.Printf("🔄 Syncing the whole inbox...\n\n")
	} else {
		duration, err := parseTimespan(timespan)
		if err != nil {
			return fmt.Errorf("invalid timespan: %w", err)
		}
		since = time.Now().Add(-duration)
		fmt.Printf("🔄 Syncing emails from the last %s...\n\n", duration)
	}

	// Step 1: Fetch and classify emails
	fmt.Println("📥 Step 1/4: Fetching and classifying emails...")
	messages, err := client.ListInboxMessagesSince(ctx, since)
	if err != nil {
		return fmt.Errorf("failed to fetch inbox messages: %w", err)
	}

	fmt.Printf("   Found %d inbox messages\n", len(messages))

	provider, err := buildLLMProvider(cfg)
	if err != nil {
		return err
	}

	classifier := newClassifier(cfg, provider, db.DB(), false) // NOT dry-run
	if err := classifier.Initialize(ctx); err != nil {
		return fmt.Errorf("failed to initialize classifier: %w", err)
	}

	archiver, err := newArchiver(cfg, client, db.DB(), classifier, dryRunLogPath)
	if err != nil {
		return err
	}
	if !archiver.Live() {
		fmt.Printf("   🧪 Archive dry-run: decisions logged to %s (no messages will be moved)\n", archiver.LogPath())
	}

	tasker, taskFallbackDue, err := newTodoistClient(cfg)
	if err != nil {
		fmt.Printf("   ⚠️  Todoist disabled: %v\n", err)
	}
	newCount := 0
	taskCreated := 0
	taskUpdated := 0

	// Step 2: Process each message
	fmt.Println("\n📋 Step 2/4: Processing messages...")
	for i, msg := range messages {
		// Check if already exists
		var existingID, existingCategory string
		err := db.DB().QueryRowContext(ctx, `SELECT id, category FROM messages WHERE id = ?`, msg.ID).Scan(&existingID, &existingCategory)

		if err == nil {
			// Already exists - re-decide (e.g. an OTP that has since been read)
			decision, err := archiver.Process(ctx, msg, existingCategory, nil)
			if err != nil {
				fmt.Printf("   ⚠️  Archive failed: %v\n", err)
			}
			triage := loadStoredTriage(ctx, db.DB(), msg.ID)
			if tasker != nil && !decision.Archive {
				status, err := upsertTodoistTask(ctx, cfg, tasker, taskFallbackDue, msg, existingCategory, triage)
				if err != nil {
					fmt.Printf("   ⚠️  Todoist failed: %v\n", err)
				} else if status == "created" {
					taskCreated++
				} else if status == "updated" {
					taskUpdated++
				}
			}
			continue
		}

		// New message - classify it
		bodyText := msg.Body.Content
		if len(bodyText) > 4000 {
			bodyText = bodyText[:4000]
		}

		email := classify.EmailInput{
			From:    msg.From.EmailAddress.Address,
			Subject: msg.Subject,
			Body:    bodyText,
		}

		result, err := classifier.Classify(ctx, email)
		if err != nil {
			fmt.Printf("   ⚠️  Failed to classify: %s\n", msg.Subject)
			continue
		}

		// Store in database
		receivedAt, _ := time.Parse(time.RFC3339, msg.ReceivedDateTime)
		_, err = db.DB().ExecContext(ctx, `
			INSERT INTO messages (id, subject, from_addr, category, received_at, raw_link, internet_message_id)
			VALUES (?, ?, ?, ?, ?, ?, ?)
		`, msg.ID, msg.Subject, msg.From.EmailAddress.Address, result.Category, receivedAt, msg.WebLink, msg.InternetMessageId)

		if err != nil {
			continue
		}

		newCount++

		// Auto-archive if it doesn't need personal attention
		decision, err := archiver.Process(ctx, msg, result.Category, result.Triage)
		if err != nil {
			fmt.Printf("   ⚠️  Archive failed: %v\n", err)
		}
		if tasker != nil && !decision.Archive {
			status, err := upsertTodoistTask(ctx, cfg, tasker, taskFallbackDue, msg, result.Category, result.Triage)
			if err != nil {
				fmt.Printf("   ⚠️  Todoist failed: %v\n", err)
			} else if status == "created" {
				taskCreated++
			} else if status == "updated" {
				taskUpdated++
			}
		}

		if (i+1)%10 == 0 {
			fmt.Printf("   Processed %d/%d...\n", i+1, len(messages))
		}
	}

	fmt.Printf("   ✓ Classified %d new messages\n", newCount)
	if archiver.Live() {
		fmt.Printf("   ✓ Archived %d messages\n\n", archiver.Archived)
	}
	if tasker != nil {
		fmt.Printf("   ✓ Todoist tasks: %d created, %d updated\n\n", taskCreated, taskUpdated)
	}

	// Step 3: Generate summaries (cached for future digests)
	digestSvc := summarize.NewDigestService(db.DB(), classifier, client)

	if !noSummary {
		fmt.Println("📝 Step 3/4: Generating email summaries...")
	} else {
		fmt.Println("📝 Step 3/4: Skipping AI summaries (using subject lines)...")
	}

	// Get all messages in the timespan that need summaries
	startTime := since
	endTime := time.Now()

	rows, err := db.DB().QueryContext(ctx, `
		SELECT m.id, m.subject, m.from_addr, m.category, m.raw_link
		FROM messages m
		LEFT JOIN email_summaries s ON m.id = s.message_id
		WHERE datetime(m.received_at) >= datetime(?)
		  AND datetime(m.received_at) < datetime(?)
		  AND s.message_id IS NULL
		  AND m.archived_at IS NULL
		ORDER BY m.received_at DESC
	`, startTime.Format(time.RFC3339), endTime.Format(time.RFC3339))

	if err != nil {
		fmt.Printf("   ⚠️  Failed to query messages: %v\n\n", err)
	} else if noSummary {
		rows.Close()
		fmt.Printf("   ✓ Skipped summary generation\n\n")
	} else {
		// Collect all messages first (don't hold cursor while processing)
		type msgInfo struct {
			id       string
			subject  string
			fromAddr string
			category string
			webLink  string
		}

		var messagesToProcess []msgInfo
		for rows.Next() {
			var msg msgInfo
			if err := rows.Scan(&msg.id, &msg.subject, &msg.fromAddr, &msg.category, &msg.webLink); err != nil {
				continue
			}
			messagesToProcess = append(messagesToProcess, msg)
		}
		rows.Close()

		totalCount := len(messagesToProcess)
		if totalCount == 0 {
			fmt.Printf("   ✓ All messages already have summaries\n\n")
		} else {
			fmt.Printf("   Found %d messages needing summaries...\n", totalCount)

			summaryCount := 0
			for i, msgInfo := range messagesToProcess {
				fmt.Printf("   [%d/%d] Generating summary for: %s\n", i+1, totalCount, msgInfo.subject)

				// Fetch message body
				msg, err := client.GetMessage(ctx, msgInfo.id)
				if err != nil {
					fmt.Printf("   ⚠️  Failed to fetch message: %v\n", err)
					continue
				}

				bodyText := msg.Body.Content
				if len(bodyText) > 1000 {
					bodyText = bodyText[:1000]
				}

				emailInput := classify.EmailInput{
					From:    msgInfo.fromAddr,
					Subject: msgInfo.subject,
					Body:    bodyText,
				}

				// Generate and cache summary
				_, err = digestSvc.GetOrCreateSummary(ctx, msgInfo.id, emailInput, msgInfo.category, msgInfo.webLink)
				if errors.Is(err, classify.ErrStudioBusy) {
					fmt.Printf("   🎛️  Music app running: deferring %d summaries to a later run\n", totalCount-i)
					break
				}
				if err == nil {
					summaryCount++
				} else {
					fmt.Printf("   ⚠️  Failed to generate summary: %v\n", err)
				}
			}
			fmt.Printf("   ✓ Generated %d email summaries\n\n", summaryCount)
		}
	}

	digest, err := digestSvc.CreateDigest(ctx, startTime, endTime)
	if err != nil {
		return fmt.Errorf("failed to create digest: %w", err)
	}

	// Step 4: Generate digest
	fmt.Println("📊 Step 4/4: Creating digest...")

	_, err = digestSvc.GenerateMetaSummary(ctx, digest)
	if err != nil {
		return fmt.Errorf("failed to generate meta summary: %w", err)
	}

	categoryOrder, err := digestSvc.GetCategoryImportance(ctx)
	if err != nil {
		return fmt.Errorf("failed to get category order: %w", err)
	}

	var markdown strings.Builder
	markdown.WriteString("# Email Digest\n\n")
	markdown.WriteString(fmt.Sprintf("**Period:** %s to %s\n\n",
		startTime.Format("2006-01-02 15:04"),
		endTime.Format("2006-01-02 15:04")))

	if digest.MetaSummary != "" {
		markdown.WriteString(digest.MetaSummary)
		markdown.WriteString("\n\n")
	}

	summarize.WriteMarkdownSections(&markdown, digest, categoryOrder)

	output := markdown.String()
	if outputFile != "" {
		if err := os.WriteFile(outputFile, []byte(output), 0644); err != nil {
			return fmt.Errorf("failed to write to file: %w", err)
		}
		fmt.Printf("   ✓ Digest saved to %s\n", outputFile)
	} else {
		fmt.Print(output)
	}

	if !archiver.Live() {
		fmt.Printf("\n🧪 Dry-run: %d messages would have been archived (logged to %s)\n", archiver.Archived, archiver.LogPath())
	}

	fmt.Println("\n✅ Sync complete!")
	return nil
}

func batchReview(ctx context.Context, cfg *Config, client *graph.Client, db *store.Store, timespan string) error {
	duration, err := parseTimespan(timespan)
	if err != nil {
		return fmt.Errorf("invalid timespan: %w", err)
	}

	fmt.Printf("Fetching emails from the last %s...\n", duration)

	// Calculate rough estimate for top parameter (assume ~25 emails per day)
	days := int(duration.Hours() / 24)
	if days < 1 {
		days = 1
	}

	// Fetch recent messages
	messages, err := client.ListMessages(ctx, days*50) // Rough estimate
	if err != nil {
		return err
	}

	provider, err := buildLLMProvider(cfg)
	if err != nil {
		return err
	}

	// Create classifier
	classifier := newClassifier(cfg, provider, db.DB(), true) // dry-run mode

	// Initialize
	if err := classifier.Initialize(ctx); err != nil {
		return fmt.Errorf("failed to initialize classifier: %w", err)
	}

	fmt.Printf("Classifying %d emails...\n", len(messages))

	categoryCounts := make(map[string]int)

	for i, msg := range messages {
		// Parse received time
		receivedAt, err := time.Parse(time.RFC3339, msg.ReceivedDateTime)
		if err != nil {
			receivedAt = time.Now()
		}

		// Check if already classified
		existingMsg, err := db.GetMessage(msg.ID)
		if err == nil && existingMsg != nil && existingMsg.Category != "" {
			// Already classified, use existing
			categoryCounts[existingMsg.Category]++
			if (i+1)%10 == 0 {
				fmt.Printf("  Processed %d/%d... (using cached classifications)\n", i+1, len(messages))
			}
			continue
		}

		// Save message to database for future feedback processing
		dbMsg := &store.Message{
			ID:         msg.ID,
			Subject:    msg.Subject,
			FromAddr:   msg.From.EmailAddress.Address,
			ReceivedAt: receivedAt,
			Importance: msg.Importance,
			IsRead:     msg.IsRead,
			RawLink:    msg.WebLink,
		}
		db.SaveMessage(dbMsg)

		// Prepare email input
		bodyText := msg.Body.Content
		if len(bodyText) > 4000 {
			bodyText = bodyText[:4000]
		}

		emailInput := classify.EmailInput{
			From:         msg.From.EmailAddress.Address,
			Subject:      msg.Subject,
			Body:         bodyText,
			Headers:      make(map[string]string),
			ToRecipients: len(msg.ToRecipients),
		}

		// Classify with AI
		result, err := classifier.Classify(ctx, emailInput)
		if err != nil {
			fmt.Printf("⚠️  Failed to classify: %s\n", msg.Subject)
			continue
		}

		// Check whitelist restrictions
		wm := classifier.GetWhitelistManager()
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

		// Update message with classification
		dbMsg.Category = result.Category
		db.SaveMessage(dbMsg)

		categoryCounts[result.Category]++

		if (i+1)%10 == 0 {
			fmt.Printf("  Processed %d/%d...\n", i+1, len(messages))
		}
	}

	// Summary
	fmt.Println("\n=== Classification Summary ===")
	for category, count := range categoryCounts {
		fmt.Printf("  %s: %d\n", category, count)
	}

	// Start review daemon with UI
	fmt.Println("\n🚀 Starting review UI...")
	archiver, err := newArchiver(cfg, client, db.DB(), classifier, "")
	if err != nil {
		return err
	}
	srv := daemon.NewServer(db.DB(), 8383, false)
	srv.SetReviewServer(client, classifier, archiver)

	fmt.Println("\n✅ Review UI ready!")
	fmt.Println("   Open: http://localhost:8383/review")
	fmt.Println("   Whitelists: http://localhost:8383/whitelists")
	fmt.Println("\nPress Ctrl+C to stop")

	return srv.Start(ctx)
}
