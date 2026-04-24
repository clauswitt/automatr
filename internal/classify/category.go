package classify

import (
	"context"
	"database/sql"
)

type CategoryManager struct {
	db *sql.DB
}

type Category struct {
	ID                  int
	Name                string
	Description         string
	SummarizationPrompt string
	KeepInInbox         bool
	PriorityDefault     string
	CreatedAt           string
	UpdatedAt           string
}

func NewCategoryManager(db *sql.DB) *CategoryManager {
	return &CategoryManager{db: db}
}

// AddCategory adds a new category
func (c *CategoryManager) AddCategory(ctx context.Context, name, description, summarizationPrompt string, keepInInbox bool, priorityDefault string) error {
	_, err := c.db.ExecContext(ctx, `
		INSERT INTO categories (name, description, summarization_prompt, keep_in_inbox, priority_default, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, datetime('now'), datetime('now'))
	`, name, description, summarizationPrompt, keepInInbox, priorityDefault)

	return err
}

// UpdateCategory updates an existing category
func (c *CategoryManager) UpdateCategory(ctx context.Context, id int, description, summarizationPrompt string, keepInInbox bool, priorityDefault string) error {
	_, err := c.db.ExecContext(ctx, `
		UPDATE categories
		SET description = ?, summarization_prompt = ?, keep_in_inbox = ?, priority_default = ?, updated_at = datetime('now')
		WHERE id = ?
	`, description, summarizationPrompt, keepInInbox, priorityDefault, id)

	return err
}

// DeleteCategory removes a category
func (c *CategoryManager) DeleteCategory(ctx context.Context, id int) error {
	_, err := c.db.ExecContext(ctx, `DELETE FROM categories WHERE id = ?`, id)
	return err
}

// ListCategories returns all categories
func (c *CategoryManager) ListCategories(ctx context.Context) ([]Category, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT id, name, description, summarization_prompt, keep_in_inbox, priority_default, created_at, updated_at
		FROM categories
		ORDER BY name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var categories []Category
	for rows.Next() {
		var cat Category
		var description, summarizationPrompt sql.NullString
		var updatedAt sql.NullString

		if err := rows.Scan(&cat.ID, &cat.Name, &description, &summarizationPrompt, &cat.KeepInInbox, &cat.PriorityDefault, &cat.CreatedAt, &updatedAt); err != nil {
			continue
		}

		cat.Description = description.String
		cat.SummarizationPrompt = summarizationPrompt.String
		cat.UpdatedAt = updatedAt.String

		categories = append(categories, cat)
	}

	return categories, rows.Err()
}

// GetCategory retrieves a single category by name
func (c *CategoryManager) GetCategory(ctx context.Context, name string) (*Category, error) {
	var cat Category
	var description, summarizationPrompt sql.NullString
	var updatedAt sql.NullString

	err := c.db.QueryRowContext(ctx, `
		SELECT id, name, description, summarization_prompt, keep_in_inbox, priority_default, created_at, updated_at
		FROM categories
		WHERE name = ?
	`, name).Scan(&cat.ID, &cat.Name, &description, &summarizationPrompt, &cat.KeepInInbox, &cat.PriorityDefault, &cat.CreatedAt, &updatedAt)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	cat.Description = description.String
	cat.SummarizationPrompt = summarizationPrompt.String
	cat.UpdatedAt = updatedAt.String

	return &cat, nil
}

// GetCategoryNames returns a list of all category names
func (c *CategoryManager) GetCategoryNames(ctx context.Context) ([]string, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT name FROM categories ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			continue
		}
		names = append(names, name)
	}

	return names, rows.Err()
}

// SeedDefaultCategories adds default categories if the table is empty
func (c *CategoryManager) SeedDefaultCategories(ctx context.Context) error {
	// Check if categories exist
	var count int
	err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM categories`).Scan(&count)
	if err != nil {
		return err
	}

	if count > 0 {
		// Already seeded — but make sure Notifications exists (added after
		// original seed; needed to route non-auth noreply@ away from Auth).
		existing, err := c.GetCategory(ctx, "Notifications")
		if err == nil && existing == nil {
			_ = c.AddCategory(ctx, "Notifications",
				"Non-auth noreply/system notifications that should stay in the inbox for quick review",
				"Briefly summarize what this notification is about and whether any action is needed.",
				true, "low")
		}
		return nil
	}

	// Default categories with descriptions and summarization prompts
	defaultCategories := []struct {
		name                string
		description         string
		summarizationPrompt string
		keepInInbox         bool
		priorityDefault     string
	}{
		{
			name:                "People",
			description:         "Direct personal communication from individuals",
			summarizationPrompt: "Start with 'ACTION NEEDED:' if there are questions to answer or tasks to complete. Then summarize: what they're asking/saying, any questions I need to respond to, deadlines mentioned, and what action I should take.",
			keepInInbox:         false,
			priorityDefault:     "high",
		},
		{
			name:                "Clients",
			description:         "Client communications and leads",
			summarizationPrompt: "Start with 'ACTION NEEDED:' if response/action required. Summarize: who (client name), what they want, any questions they asked, deadlines, and what I need to do next.",
			keepInInbox:         true,
			priorityDefault:     "high",
		},
		{
			name:                "Finance",
			description:         "Receipts, invoices, and financial notifications",
			summarizationPrompt: "If action required (payment due, review needed), start with 'ACTION NEEDED:' and the deadline. Then: amount, merchant/service, payment method, and what I need to do.",
			keepInInbox:         false,
			priorityDefault:     "medium",
		},
		{
			name:                "Auth",
			description:         "Authentication codes and security notifications",
			summarizationPrompt: "Extract authentication code if present. Note the service it's for and expiration time if mentioned.",
			keepInInbox:         false,
			priorityDefault:     "high",
		},
		{
			name:                "Marketing",
			description:         "Promotional emails and newsletters",
			summarizationPrompt: "Brief summary of the promotion or newsletter topic. This is typically low priority.",
			keepInInbox:         false,
			priorityDefault:     "spam",
		},
		{
			name:                "Product Updates",
			description:         "Software updates and service announcements",
			summarizationPrompt: "If action required (update needed, breaking changes), start with 'ACTION NEEDED:'. Summarize: which product/service, what changed, impact to my usage, and what I need to do.",
			keepInInbox:         false,
			priorityDefault:     "medium",
		},
		{
			name:                "Ops",
			description:         "Operational alerts and system notifications",
			summarizationPrompt: "If urgent action needed, start with 'ACTION NEEDED:'. Summarize: which system/service, what happened, severity/impact, and what I should do about it. Highlight cost changes and anomalies.",
			keepInInbox:         true,
			priorityDefault:     "high",
		},
		{
			name:                "Community",
			description:         "Forum posts, social notifications",
			summarizationPrompt: "Brief summary of community activity: platform, topic, who mentioned you or what you were tagged in.",
			keepInInbox:         false,
			priorityDefault:     "low",
		},
		{
			name:                "Projects",
			description:         "Project management and collaboration updates",
			summarizationPrompt: "If I'm assigned a task or someone asked a question, start with 'ACTION NEEDED:'. Summarize: project name, what changed, who did it, any questions for me, and what I need to do.",
			keepInInbox:         false,
			priorityDefault:     "medium",
		},
		{
			name:                "Later",
			description:         "Low priority items to review later",
			summarizationPrompt: "Brief summary of what this is about and why it was marked as later.",
			keepInInbox:         false,
			priorityDefault:     "low",
		},
		{
			name:                "Other",
			description:         "Uncategorized or unclear emails",
			summarizationPrompt: "If there are questions or action items, start with 'ACTION NEEDED:'. Summarize: what it's about, any questions to answer, and what I should do.",
			keepInInbox:         false,
			priorityDefault:     "medium",
		},
		{
			name:                "Notifications",
			description:         "Non-auth noreply/system notifications that should stay in the inbox for quick review",
			summarizationPrompt: "Briefly summarize what this notification is about and whether any action is needed.",
			keepInInbox:         true,
			priorityDefault:     "low",
		},
	}

	for _, cat := range defaultCategories {
		if err := c.AddCategory(ctx, cat.name, cat.description, cat.summarizationPrompt, cat.keepInInbox, cat.priorityDefault); err != nil {
			return err
		}
	}

	return nil
}
