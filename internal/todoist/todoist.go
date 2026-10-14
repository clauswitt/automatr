package todoist

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

const baseURL = "https://api.todoist.com/api/v1"

type Client struct {
	token      string
	httpClient *http.Client
}

func NewClient(token string) *Client {
	return &Client{
		token:      token,
		httpClient: &http.Client{Timeout: 20 * time.Second},
	}
}

type Task struct {
	ID          string `json:"id"`
	Content     string `json:"content"`
	Description string `json:"description"`
}

type EmailTask struct {
	Key        string
	Subject    string
	From       string
	ReceivedAt time.Time
	WebLink    string
	Body       string
	DueDate    string
	Priority   int
	ProjectID  string
	Labels     []string
	Reason     string
}

func (c *Client) UpsertEmailTask(ctx context.Context, task EmailTask) (string, error) {
	if c.token == "" {
		return "", fmt.Errorf("todoist token is empty")
	}
	if task.Key == "" {
		task.Key = Key(task.From, task.Subject)
	}
	marker := marker(task.Key)

	tasks, err := c.listTasks(ctx)
	if err != nil {
		return "", err
	}
	for _, existing := range tasks {
		if strings.Contains(existing.Description, marker) {
			desc := appendEmailLink(existing.Description, task)
			if desc == existing.Description {
				return "unchanged", nil
			}
			if err := c.updateTask(ctx, existing.ID, map[string]any{"description": desc}); err != nil {
				return "", err
			}
			return "updated", nil
		}
	}

	payload := map[string]any{
		"content":     "Email: " + task.Subject,
		"description": newDescription(marker, task),
		"priority":    task.Priority,
	}
	if task.ProjectID != "" {
		payload["project_id"] = task.ProjectID
	}
	if task.DueDate != "" {
		payload["due_date"] = task.DueDate
	}
	if len(task.Labels) > 0 {
		payload["labels"] = task.Labels
	}
	if err := c.createTask(ctx, payload); err != nil {
		return "", err
	}
	return "created", nil
}

func Key(from, subject string) string {
	normalized := normalizeSubject(subject)
	host := from
	if i := strings.LastIndex(host, "@"); i >= 0 {
		host = host[i+1:]
	}
	sum := sha1.Sum([]byte(strings.ToLower(host) + "|" + normalized))
	return hex.EncodeToString(sum[:])[:16]
}

func InferDueDate(now time.Time, subject, body string, fallback time.Duration) string {
	text := subject + "\n" + body
	if due, ok := findExplicitDate(now, text); ok {
		return due.Format("2006-01-02")
	}
	if fallback <= 0 {
		fallback = 48 * time.Hour
	}
	return now.Add(fallback).Format("2006-01-02")
}

func normalizeSubject(subject string) string {
	s := strings.ToLower(strings.TrimSpace(subject))
	prefix := regexp.MustCompile(`^((re|fw|fwd|sv|vs):\s*)+`)
	s = prefix.ReplaceAllString(s, "")
	s = regexp.MustCompile(`\s+`).ReplaceAllString(s, " ")
	s = regexp.MustCompile(`\b\d+\b`).ReplaceAllString(s, "#")
	return strings.TrimSpace(s)
}

func marker(key string) string {
	return "automatr-email-key: " + key
}

func newDescription(marker string, task EmailTask) string {
	var b strings.Builder
	b.WriteString(marker)
	b.WriteString("\n")
	if task.Reason != "" {
		b.WriteString("Reason: ")
		b.WriteString(task.Reason)
		b.WriteString("\n")
	}
	b.WriteString("\nEmail links:\n")
	b.WriteString(emailBullet(task))
	return b.String()
}

func appendEmailLink(desc string, task EmailTask) string {
	if task.WebLink != "" && strings.Contains(desc, task.WebLink) {
		return desc
	}
	bullet := emailBullet(task)
	if strings.Contains(desc, "\nEmail links:\n") {
		return strings.TrimRight(desc, "\n") + "\n" + bullet
	}
	return strings.TrimRight(desc, "\n") + "\n\nEmail links:\n" + bullet
}

func emailBullet(task EmailTask) string {
	date := task.ReceivedAt.Format("2006-01-02")
	if task.WebLink == "" {
		return fmt.Sprintf("- %s · %s — %s\n", date, task.From, task.Subject)
	}
	return fmt.Sprintf("- %s · %s — [%s](%s)\n", date, task.From, task.Subject, task.WebLink)
}

func findExplicitDate(now time.Time, text string) (time.Time, bool) {
	monthNames := map[string]time.Month{
		"jan": time.January, "january": time.January,
		"feb": time.February, "february": time.February,
		"mar": time.March, "march": time.March,
		"apr": time.April, "april": time.April,
		"may": time.May,
		"jun": time.June, "june": time.June,
		"jul": time.July, "july": time.July,
		"aug": time.August, "august": time.August,
		"sep": time.September, "sept": time.September, "september": time.September,
		"oct": time.October, "october": time.October,
		"nov": time.November, "november": time.November,
		"dec": time.December, "december": time.December,
	}
	re := regexp.MustCompile(`(?i)\b(jan(?:uary)?|feb(?:ruary)?|mar(?:ch)?|apr(?:il)?|may|jun(?:e)?|jul(?:y)?|aug(?:ust)?|sep(?:t|tember)?|oct(?:ober)?|nov(?:ember)?|dec(?:ember)?)\.?\s+([0-3]?\d)(?:st|nd|rd|th)?(?:,\s*(20\d{2}))?\b`)
	matches := re.FindAllStringSubmatch(text, -1)
	var dates []time.Time
	for _, m := range matches {
		month := monthNames[strings.ToLower(strings.TrimSuffix(m[1], "."))]
		day := atoi(m[2])
		year := now.Year()
		if m[3] != "" {
			year = atoi(m[3])
		}
		d := time.Date(year, month, day, 12, 0, 0, 0, now.Location())
		if m[3] == "" && d.Before(now.AddDate(0, 0, -1)) {
			d = d.AddDate(1, 0, 0)
		}
		if !d.Before(now.AddDate(0, 0, -1)) {
			dates = append(dates, d)
		}
	}
	if len(dates) == 0 {
		return time.Time{}, false
	}
	sort.Slice(dates, func(i, j int) bool { return dates[i].Before(dates[j]) })
	return dates[0], true
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r >= '0' && r <= '9' {
			n = n*10 + int(r-'0')
		}
	}
	return n
}

func (c *Client) listTasks(ctx context.Context) ([]Task, error) {
	var all []Task
	cursor := ""
	for {
		url := baseURL + "/tasks?limit=200"
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		var page struct {
			Results    []Task  `json:"results"`
			NextCursor *string `json:"next_cursor"`
		}
		if err := c.do(ctx, http.MethodGet, url, nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Results...)
		if page.NextCursor == nil || *page.NextCursor == "" {
			return all, nil
		}
		cursor = *page.NextCursor
	}
}

func (c *Client) createTask(ctx context.Context, payload map[string]any) error {
	return c.do(ctx, http.MethodPost, baseURL+"/tasks", payload, nil)
}

func (c *Client) updateTask(ctx context.Context, id string, payload map[string]any) error {
	return c.do(ctx, http.MethodPost, baseURL+"/tasks/"+id, payload, nil)
}

func (c *Client) do(ctx context.Context, method, url string, payload any, out any) error {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("todoist API %d: %s", resp.StatusCode, string(data))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
