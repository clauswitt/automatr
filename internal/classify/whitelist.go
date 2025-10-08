package classify

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
)

type WhitelistManager struct {
	db *sql.DB
}

type Whitelist struct {
	ID                int
	Pattern           string
	PatternType       string   // "email" or "domain"
	AllowedCategories []string // nil = all categories allowed
	AutoArchive       bool
	CreatedAt         string
}

func NewWhitelistManager(db *sql.DB) *WhitelistManager {
	return &WhitelistManager{db: db}
}

// AddWhitelist adds a new whitelist entry
func (w *WhitelistManager) AddWhitelist(ctx context.Context, pattern, patternType string, allowedCategories []string, autoArchive bool) error {
	categoriesJSON := ""
	if len(allowedCategories) > 0 {
		data, err := json.Marshal(allowedCategories)
		if err != nil {
			return err
		}
		categoriesJSON = string(data)
	}

	_, err := w.db.ExecContext(ctx, `
		INSERT INTO whitelists (pattern, pattern_type, allowed_categories, auto_archive, created_at)
		VALUES (?, ?, ?, ?, datetime('now'))
	`, pattern, patternType, categoriesJSON, autoArchive)

	return err
}

// RemoveWhitelist removes a whitelist entry
func (w *WhitelistManager) RemoveWhitelist(ctx context.Context, id int) error {
	_, err := w.db.ExecContext(ctx, `DELETE FROM whitelists WHERE id = ?`, id)
	return err
}

// CheckWhitelist checks if an email address matches any whitelist
func (w *WhitelistManager) CheckWhitelist(ctx context.Context, emailAddr string) (*Whitelist, error) {
	rows, err := w.db.QueryContext(ctx, `
		SELECT id, pattern, pattern_type, allowed_categories, auto_archive, created_at
		FROM whitelists
		ORDER BY pattern_type DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var wl Whitelist
		var categoriesJSON string

		if err := rows.Scan(&wl.ID, &wl.Pattern, &wl.PatternType, &categoriesJSON, &wl.AutoArchive, &wl.CreatedAt); err != nil {
			continue
		}

		// Parse allowed categories
		if categoriesJSON != "" {
			if err := json.Unmarshal([]byte(categoriesJSON), &wl.AllowedCategories); err != nil {
				continue
			}
		}

		// Check if email matches this whitelist
		if w.matches(emailAddr, wl.Pattern, wl.PatternType) {
			return &wl, nil
		}
	}

	return nil, nil
}

// matches checks if an email address matches a whitelist pattern
func (w *WhitelistManager) matches(emailAddr, pattern, patternType string) bool {
	emailAddr = strings.ToLower(emailAddr)
	pattern = strings.ToLower(pattern)

	switch patternType {
	case "email":
		return emailAddr == pattern
	case "domain":
		// Extract domain from email
		parts := strings.Split(emailAddr, "@")
		if len(parts) != 2 {
			return false
		}
		return parts[1] == pattern
	default:
		return false
	}
}

// ListWhitelists returns all whitelist entries
func (w *WhitelistManager) ListWhitelists(ctx context.Context) ([]Whitelist, error) {
	rows, err := w.db.QueryContext(ctx, `
		SELECT id, pattern, pattern_type, allowed_categories, auto_archive, created_at
		FROM whitelists
		ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var whitelists []Whitelist
	for rows.Next() {
		var wl Whitelist
		var categoriesJSON string

		if err := rows.Scan(&wl.ID, &wl.Pattern, &wl.PatternType, &categoriesJSON, &wl.AutoArchive, &wl.CreatedAt); err != nil {
			continue
		}

		if categoriesJSON != "" {
			json.Unmarshal([]byte(categoriesJSON), &wl.AllowedCategories)
		}

		whitelists = append(whitelists, wl)
	}

	return whitelists, rows.Err()
}

// FilterAllowedCategory checks if a category is allowed for a whitelist
func (w *Whitelist) FilterAllowedCategory(category string) string {
	// If no restrictions, allow any category
	if len(w.AllowedCategories) == 0 {
		return category
	}

	// Check if the category is in allowed list
	for _, allowed := range w.AllowedCategories {
		if allowed == category {
			return category
		}
	}

	// If only one category allowed, use that
	if len(w.AllowedCategories) == 1 {
		return w.AllowedCategories[0]
	}

	// Multiple categories allowed but current doesn't match - keep original
	// (LLM should choose from allowed categories)
	return category
}

// UpdateWhitelist updates an existing whitelist entry
func (w *WhitelistManager) UpdateWhitelist(ctx context.Context, id int, allowedCategories []string, autoArchive bool) error {
	categoriesJSON := ""
	if len(allowedCategories) > 0 {
		data, err := json.Marshal(allowedCategories)
		if err != nil {
			return err
		}
		categoriesJSON = string(data)
	}

	_, err := w.db.ExecContext(ctx, `
		UPDATE whitelists
		SET allowed_categories = ?, auto_archive = ?
		WHERE id = ?
	`, categoriesJSON, autoArchive, id)

	return err
}
