// Package archive applies the classify.ArchivePolicy to inbox messages and
// either moves them to Archive or, in dry-run mode, logs what it would do.
package archive

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/clauswitt/automatr/internal/classify"
	"github.com/clauswitt/automatr/internal/graph"
)

type Archiver struct {
	db         *sql.DB
	client     *graph.Client
	classifier *classify.Classifier
	policy     classify.ArchivePolicy
	live       bool
	logPath    string

	mu              sync.Mutex
	archiveFolderID string

	// Archived counts messages moved (live) or logged as would-archive (dry-run).
	Archived int
}

func New(db *sql.DB, client *graph.Client, classifier *classify.Classifier, policy classify.ArchivePolicy, live bool, logPath string) *Archiver {
	return &Archiver{db: db, client: client, classifier: classifier, policy: policy, live: live, logPath: logPath}
}

func (a *Archiver) Live() bool                     { return a.live }
func (a *Archiver) LogPath() string                { return a.logPath }
func (a *Archiver) Policy() classify.ArchivePolicy { return a.policy }

// Process decides and acts on one inbox message already stored under msg.ID.
// triage is the result from classification; pass nil for messages seen in an
// earlier sync and the stored triage is used (or fetched once if missing).
func (a *Archiver) Process(ctx context.Context, msg graph.Message, category string, triage *classify.Triage) (classify.ArchiveDecision, error) {
	if triage != nil {
		a.saveTriage(ctx, msg.ID, triage)
	} else {
		triage = a.loadOrFetchTriage(ctx, msg)
	}

	receivedAt, _ := time.Parse(time.RFC3339, msg.ReceivedDateTime)
	decision := a.policy.Decide(triage, classify.MessageState{
		From:       msg.From.EmailAddress.Address,
		IsRead:     msg.IsRead,
		Flagged:    msg.Flag.FlagStatus == "flagged",
		Importance: msg.Importance,
		ReceivedAt: receivedAt,
	}, time.Now())

	if !decision.Archive {
		return decision, nil
	}

	if !a.live {
		return decision, a.logWouldArchive(category, decision.Reason, msg)
	}

	folderID, err := a.folderID(ctx)
	if err != nil {
		return decision, err
	}
	moved, err := a.client.MoveMessage(ctx, msg.ID, folderID)
	if err != nil {
		return decision, fmt.Errorf("failed to move %q: %w", msg.Subject, err)
	}
	// The message ID changes when it moves folders
	_, err = a.db.ExecContext(ctx, `UPDATE messages SET id = ?, raw_link = ?, archived_at = datetime('now') WHERE id = ?`,
		moved.ID, moved.WebLink, msg.ID)
	a.Archived++
	return decision, err
}

func (a *Archiver) saveTriage(ctx context.Context, id string, t *classify.Triage) {
	_, _ = a.db.ExecContext(ctx, `UPDATE messages SET needs_me = ?, time_bound = ? WHERE id = ?`, t.NeedsMe, t.TimeBound, id)
}

func (a *Archiver) loadOrFetchTriage(ctx context.Context, msg graph.Message) *classify.Triage {
	var needsMe, timeBound sql.NullFloat64
	err := a.db.QueryRowContext(ctx, `SELECT needs_me, time_bound FROM messages WHERE id = ?`, msg.ID).Scan(&needsMe, &timeBound)
	if err == nil && needsMe.Valid && timeBound.Valid {
		return &classify.Triage{NeedsMe: needsMe.Float64, TimeBound: timeBound.Float64}
	}

	// Classified before triage existed: ask once and remember the answer
	t, err := a.classifier.Triage(ctx, classify.EmailInput{
		From:    msg.From.EmailAddress.Address,
		Subject: msg.Subject,
		Body:    msg.Body.Content,
	})
	if err != nil {
		return nil
	}
	a.saveTriage(ctx, msg.ID, t)
	return t
}

func (a *Archiver) folderID(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.archiveFolderID == "" {
		id, err := a.client.GetFolder(ctx, "Archive")
		if err != nil {
			return "", fmt.Errorf("failed to resolve Archive folder: %w", err)
		}
		a.archiveFolderID = id
	}
	return a.archiveFolderID, nil
}

func (a *Archiver) logWouldArchive(category, reason string, msg graph.Message) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	f, err := os.OpenFile(a.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to open dry-run log %s: %w", a.logPath, err)
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "[WOULD-ARCHIVE] %s | %s | %s | %s | %s\n",
		time.Now().Format(time.RFC3339), category, reason, msg.From.EmailAddress.Address, msg.Subject)
	a.Archived++
	return err
}
