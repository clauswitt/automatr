package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Store struct {
	db *sql.DB
}

// DB returns the underlying database connection
func (s *Store) DB() *sql.DB {
	return s.db
}

type Message struct {
	ID           string
	Subject      string
	FromAddr     string
	ReceivedAt   time.Time
	Importance   string
	IsRead       bool
	Category     string
	Tags         string
	ArchivedAt   *time.Time
	RawLink      string
}

type FolderSync struct {
	FolderID     string
	DeltaToken   string
	LastSyncedAt time.Time
}

func New(dbPath string) (*Store, error) {
	// Expand home directory
	if dbPath[:2] == "~/" {
		home, _ := os.UserHomeDir()
		dbPath = filepath.Join(home, dbPath[2:])
	}

	// Ensure directory exists
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create db directory: %w", err)
	}

	// Open with WAL mode and busy timeout for better concurrent access
	db, err := sql.Open("sqlite3", dbPath+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// SQLite works best with a single connection
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	store := &Store{db: db}
	if err := store.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to run migrations: %w", err)
	}

	return store, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	schema := `
	CREATE TABLE IF NOT EXISTS state (
		key TEXT PRIMARY KEY,
		value TEXT
	);

	CREATE TABLE IF NOT EXISTS messages (
		id TEXT PRIMARY KEY,
		subject TEXT,
		from_addr TEXT,
		received_at DATETIME,
		importance TEXT,
		is_read INTEGER,
		category TEXT,
		tags TEXT,
		archived_at DATETIME,
		raw_link TEXT
	);

	CREATE TABLE IF NOT EXISTS folder_sync (
		folder_id TEXT PRIMARY KEY,
		delta_token TEXT,
		last_synced_at DATETIME
	);

	CREATE TABLE IF NOT EXISTS deliveries (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		window_start DATETIME,
		window_end DATETIME,
		created_at DATETIME
	);

	CREATE TABLE IF NOT EXISTS embeddings (
		text_hash TEXT PRIMARY KEY,
		embedding TEXT NOT NULL,
		created_at DATETIME
	);

	CREATE TABLE IF NOT EXISTS classification_cache (
		message_id TEXT PRIMARY KEY,
		category TEXT,
		confidence REAL,
		method TEXT,
		reasoning TEXT,
		classified_at DATETIME
	);

	CREATE TABLE IF NOT EXISTS user_feedback (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		message_id TEXT NOT NULL,
		action TEXT NOT NULL,
		category TEXT,
		priority TEXT,
		reason TEXT,
		created_at DATETIME,
		applied INTEGER DEFAULT 0
	);

	CREATE TABLE IF NOT EXISTS learned_rules (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		pattern TEXT NOT NULL,
		pattern_type TEXT NOT NULL,
		category TEXT,
		priority TEXT,
		confidence REAL,
		source TEXT,
		feedback_count INTEGER DEFAULT 1,
		created_at DATETIME,
		last_applied DATETIME
	);

	CREATE TABLE IF NOT EXISTS learned_examples (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		category TEXT NOT NULL,
		text TEXT NOT NULL,
		embedding TEXT NOT NULL,
		created_at DATETIME
	);

	CREATE TABLE IF NOT EXISTS whitelists (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		pattern TEXT NOT NULL UNIQUE,
		pattern_type TEXT NOT NULL,
		allowed_categories TEXT,
		auto_archive INTEGER DEFAULT 0,
		created_at DATETIME
	);

	CREATE TABLE IF NOT EXISTS categories (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		description TEXT,
		summarization_prompt TEXT,
		keep_in_inbox INTEGER DEFAULT 0,
		priority_default TEXT DEFAULT 'medium',
		created_at DATETIME,
		updated_at DATETIME
	);

	CREATE TABLE IF NOT EXISTS email_summaries (
		message_id TEXT PRIMARY KEY,
		summary TEXT NOT NULL,
		created_at DATETIME,
		FOREIGN KEY (message_id) REFERENCES messages(id)
	);

	CREATE TABLE IF NOT EXISTS digest_summaries (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		start_time DATETIME NOT NULL,
		end_time DATETIME NOT NULL,
		meta_summary TEXT,
		created_at DATETIME
	);

	CREATE INDEX IF NOT EXISTS idx_messages_received_at ON messages(received_at);
	CREATE INDEX IF NOT EXISTS idx_messages_category ON messages(category);
	CREATE INDEX IF NOT EXISTS idx_embeddings_created_at ON embeddings(created_at);
	CREATE INDEX IF NOT EXISTS idx_user_feedback_message ON user_feedback(message_id);
	CREATE INDEX IF NOT EXISTS idx_user_feedback_applied ON user_feedback(applied);
	CREATE INDEX IF NOT EXISTS idx_learned_rules_pattern ON learned_rules(pattern);
	`

	if _, err := s.db.Exec(schema); err != nil {
		return err
	}

	// Migrations for existing databases
	return s.runMigrations()
}

func (s *Store) runMigrations() error {
	// Check if pattern_type column exists in learned_rules
	var hasPatternType bool
	rows, err := s.db.Query("PRAGMA table_info(learned_rules)")
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name, colType string
		var notNull, pk int
		var dfltValue sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			continue
		}
		if name == "pattern_type" {
			hasPatternType = true
			break
		}
	}
	// Release the connection before running more statements (single-connection pool)
	rows.Close()

	// Add pattern_type column if it doesn't exist
	if !hasPatternType {
		_, err := s.db.Exec(`
			ALTER TABLE learned_rules ADD COLUMN pattern_type TEXT DEFAULT 'sender_domain';
		`)
		if err != nil {
			return fmt.Errorf("failed to add pattern_type column: %w", err)
		}
	}

	// Jev triage probabilities, kept so later syncs can re-decide (e.g. release
	// an OTP once it has been read) without asking Jev again.
	for _, col := range []string{"needs_me REAL", "time_bound REAL", "cold_outreach REAL"} {
		if err := s.addColumnIfMissing("messages", col); err != nil {
			return err
		}
	}

	return nil
}

// addColumnIfMissing adds a column ("name TYPE") to table unless it exists.
func (s *Store) addColumnIfMissing(table, columnDef string) error {
	name := strings.Fields(columnDef)[0]
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, name).Scan(&count)
	if err != nil {
		return fmt.Errorf("failed to inspect %s: %w", table, err)
	}
	if count > 0 {
		return nil
	}
	if _, err := s.db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s", table, columnDef)); err != nil {
		return fmt.Errorf("failed to add %s.%s: %w", table, name, err)
	}
	return nil
}

// State methods
func (s *Store) GetState(key string) (string, error) {
	var value string
	err := s.db.QueryRow("SELECT value FROM state WHERE key = ?", key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

func (s *Store) SetState(key, value string) error {
	_, err := s.db.Exec(
		"INSERT OR REPLACE INTO state (key, value) VALUES (?, ?)",
		key, value,
	)
	return err
}

// Message methods
func (s *Store) SaveMessage(msg *Message) error {
	_, err := s.db.Exec(`
		INSERT OR REPLACE INTO messages
		(id, subject, from_addr, received_at, importance, is_read, category, tags, archived_at, raw_link)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		msg.ID,
		msg.Subject,
		msg.FromAddr,
		msg.ReceivedAt,
		msg.Importance,
		msg.IsRead,
		msg.Category,
		msg.Tags,
		msg.ArchivedAt,
		msg.RawLink,
	)
	return err
}

func (s *Store) GetMessage(id string) (*Message, error) {
	var msg Message
	err := s.db.QueryRow(`
		SELECT id, subject, from_addr, received_at, importance, is_read, category, tags, archived_at, raw_link
		FROM messages WHERE id = ?
	`, id).Scan(
		&msg.ID,
		&msg.Subject,
		&msg.FromAddr,
		&msg.ReceivedAt,
		&msg.Importance,
		&msg.IsRead,
		&msg.Category,
		&msg.Tags,
		&msg.ArchivedAt,
		&msg.RawLink,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &msg, err
}

func (s *Store) ListMessages(limit int) ([]*Message, error) {
	rows, err := s.db.Query(`
		SELECT id, subject, from_addr, received_at, importance, is_read, category, tags, archived_at, raw_link
		FROM messages
		ORDER BY received_at DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []*Message
	for rows.Next() {
		var msg Message
		if err := rows.Scan(
			&msg.ID,
			&msg.Subject,
			&msg.FromAddr,
			&msg.ReceivedAt,
			&msg.Importance,
			&msg.IsRead,
			&msg.Category,
			&msg.Tags,
			&msg.ArchivedAt,
			&msg.RawLink,
		); err != nil {
			return nil, err
		}
		messages = append(messages, &msg)
	}

	return messages, rows.Err()
}

// Folder sync methods
func (s *Store) GetFolderSync(folderID string) (*FolderSync, error) {
	var fs FolderSync
	err := s.db.QueryRow(`
		SELECT folder_id, delta_token, last_synced_at
		FROM folder_sync WHERE folder_id = ?
	`, folderID).Scan(&fs.FolderID, &fs.DeltaToken, &fs.LastSyncedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &fs, err
}

func (s *Store) SaveFolderSync(fs *FolderSync) error {
	_, err := s.db.Exec(`
		INSERT OR REPLACE INTO folder_sync (folder_id, delta_token, last_synced_at)
		VALUES (?, ?, ?)
	`, fs.FolderID, fs.DeltaToken, fs.LastSyncedAt)
	return err
}
