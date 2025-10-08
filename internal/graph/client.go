package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const baseURL = "https://graph.microsoft.com/v1.0"

type Client struct {
	accessToken string
	httpClient  *http.Client
}

func NewClient(accessToken string) *Client {
	return &Client{
		accessToken: accessToken,
		httpClient:  &http.Client{Timeout: 30 * time.Second},
	}
}

// Message represents an Outlook message
type Message struct {
	ID                 string    `json:"id"`
	InternetMessageId  string    `json:"internetMessageId"`
	Subject            string    `json:"subject"`
	ReceivedDateTime   string    `json:"receivedDateTime"`
	Importance         string    `json:"importance"`
	IsRead             bool      `json:"isRead"`
	HasAttachments     bool      `json:"hasAttachments"`
	From               Recipient `json:"from"`
	ToRecipients       []Recipient `json:"toRecipients"`
	CcRecipients       []Recipient `json:"ccRecipients"`
	Body               Body      `json:"body"`
	Categories         []string  `json:"categories"`
	WebLink            string    `json:"webLink"`
}

type Recipient struct {
	EmailAddress EmailAddress `json:"emailAddress"`
}

type EmailAddress struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

type Body struct {
	ContentType string `json:"contentType"`
	Content     string `json:"content"`
}

type MessageList struct {
	Value         []Message `json:"value"`
	NextLink      string    `json:"@odata.nextLink"`
	DeltaLink     string    `json:"@odata.deltaLink"`
}

// ListMessages retrieves the most recent messages from the inbox
func (c *Client) ListMessages(ctx context.Context, top int) ([]Message, error) {
	endpoint := fmt.Sprintf("%s/me/mailFolders/inbox/messages?$top=%d&$orderby=receivedDateTime desc", baseURL, top)

	var result MessageList
	if err := c.get(ctx, endpoint, &result); err != nil {
		return nil, err
	}

	return result.Value, nil
}

// ListRecentMessages retrieves messages received after a specific time from inbox and archive folders
func (c *Client) ListRecentMessages(ctx context.Context, since time.Time) ([]Message, error) {
	// Format time in ISO 8601 format for Graph API filter
	sinceStr := since.UTC().Format("2006-01-02T15:04:05Z")
	filter := fmt.Sprintf("receivedDateTime ge %s", sinceStr)

	var allMessages []Message

	// Query inbox folder
	inboxEndpoint := fmt.Sprintf("%s/me/mailFolders/inbox/messages?$filter=%s&$orderby=receivedDateTime desc&$top=999",
		baseURL, url.QueryEscape(filter))

	for inboxEndpoint != "" {
		var result MessageList
		if err := c.get(ctx, inboxEndpoint, &result); err != nil {
			return nil, err
		}
		allMessages = append(allMessages, result.Value...)
		inboxEndpoint = result.NextLink
	}

	// Query archive folder
	archiveID, err := c.GetFolder(ctx, "Archive")
	if err == nil {
		archiveEndpoint := fmt.Sprintf("%s/me/mailFolders/%s/messages?$filter=%s&$orderby=receivedDateTime desc&$top=999",
			baseURL, archiveID, url.QueryEscape(filter))

		for archiveEndpoint != "" {
			var result MessageList
			if err := c.get(ctx, archiveEndpoint, &result); err != nil {
				break // Skip archive errors
			}
			allMessages = append(allMessages, result.Value...)
			archiveEndpoint = result.NextLink
		}
	}

	return allMessages, nil
}

// ListInboxMessagesSince retrieves messages received after a specific time from INBOX ONLY
func (c *Client) ListInboxMessagesSince(ctx context.Context, since time.Time) ([]Message, error) {
	// Format time in ISO 8601 format for Graph API filter
	sinceStr := since.UTC().Format("2006-01-02T15:04:05Z")
	filter := fmt.Sprintf("receivedDateTime ge %s", sinceStr)

	var allMessages []Message

	// Query ONLY inbox folder
	endpoint := fmt.Sprintf("%s/me/mailFolders/inbox/messages?$filter=%s&$orderby=receivedDateTime desc&$top=999",
		baseURL, url.QueryEscape(filter))

	for endpoint != "" {
		var result MessageList
		if err := c.get(ctx, endpoint, &result); err != nil {
			return nil, err
		}
		allMessages = append(allMessages, result.Value...)
		endpoint = result.NextLink
	}

	return allMessages, nil
}

// SearchMessages searches for messages by subject or sender
func (c *Client) SearchMessages(ctx context.Context, query string) ([]Message, error) {
	endpoint := fmt.Sprintf("%s/me/messages?$search=\"%s\"&$top=20", baseURL, url.QueryEscape(query))

	var result MessageList
	if err := c.get(ctx, endpoint, &result); err != nil {
		return nil, err
	}

	return result.Value, nil
}

// ListUnreadMessages retrieves all recent messages (not just inbox)
func (c *Client) ListUnreadMessages(ctx context.Context, top int) ([]Message, error) {
	endpoint := fmt.Sprintf("%s/me/messages?$top=%d&$orderby=receivedDateTime DESC", baseURL, top)

	var result MessageList
	if err := c.get(ctx, endpoint, &result); err != nil {
		return nil, err
	}

	return result.Value, nil
}

// GetMessage retrieves a specific message by ID
func (c *Client) GetMessage(ctx context.Context, messageID string) (*Message, error) {
	endpoint := fmt.Sprintf("%s/me/messages/%s", baseURL, messageID)

	var message Message
	if err := c.get(ctx, endpoint, &message); err != nil {
		return nil, err
	}

	return &message, nil
}

// GetDeltaMessages retrieves messages using delta query
func (c *Client) GetDeltaMessages(ctx context.Context, deltaLink string) (*MessageList, error) {
	endpoint := deltaLink
	if deltaLink == "" {
		endpoint = fmt.Sprintf("%s/me/mailFolders/inbox/messages/delta", baseURL)
	}

	var result MessageList
	if err := c.get(ctx, endpoint, &result); err != nil {
		return nil, err
	}

	return &result, nil
}

// ListFolders retrieves all mail folders
func (c *Client) ListFolders(ctx context.Context) ([]Folder, error) {
	endpoint := fmt.Sprintf("%s/me/mailFolders", baseURL)

	var result struct {
		Value []Folder `json:"value"`
	}

	if err := c.get(ctx, endpoint, &result); err != nil {
		return nil, err
	}

	return result.Value, nil
}

// GetFolder retrieves a folder by name
func (c *Client) GetFolder(ctx context.Context, folderName string) (string, error) {
	// Construct filter with proper encoding
	filter := fmt.Sprintf("displayName eq '%s'", folderName)
	endpoint := fmt.Sprintf("%s/me/mailFolders?$filter=%s", baseURL, url.QueryEscape(filter))

	var result struct {
		Value []struct {
			ID          string `json:"id"`
			DisplayName string `json:"displayName"`
		} `json:"value"`
	}

	if err := c.get(ctx, endpoint, &result); err != nil {
		return "", err
	}

	if len(result.Value) == 0 {
		return "", fmt.Errorf("folder '%s' not found", folderName)
	}

	return result.Value[0].ID, nil
}

type Folder struct {
	ID                    string `json:"id"`
	DisplayName           string `json:"displayName"`
	TotalItemCount        int    `json:"totalItemCount"`
	UnreadItemCount       int    `json:"unreadItemCount"`
	ChildFolderCount      int    `json:"childFolderCount"`
}

// GetMessageByInternetMessageId retrieves a message by its Internet Message-ID
// This is useful for finding messages after they've been moved/archived
func (c *Client) GetMessageByInternetMessageId(ctx context.Context, internetMessageId string) (*Message, error) {
	if internetMessageId == "" {
		return nil, fmt.Errorf("internetMessageId cannot be empty")
	}

	// Remove angle brackets if present
	msgID := internetMessageId
	if len(msgID) > 0 && msgID[0] == '<' {
		msgID = msgID[1:]
	}
	if len(msgID) > 0 && msgID[len(msgID)-1] == '>' {
		msgID = msgID[:len(msgID)-1]
	}

	filter := fmt.Sprintf("internetMessageId eq '%s'", msgID)
	endpoint := fmt.Sprintf("%s/me/messages?$filter=%s&$top=1", baseURL, url.QueryEscape(filter))

	var result MessageList
	if err := c.get(ctx, endpoint, &result); err != nil {
		return nil, err
	}

	if len(result.Value) == 0 {
		return nil, fmt.Errorf("message not found with internetMessageId: %s", internetMessageId)
	}

	return &result.Value[0], nil
}

// MoveMessage moves a message to a different folder and returns the updated message with new webLink
func (c *Client) MoveMessage(ctx context.Context, messageID, destinationFolderID string) (*Message, error) {
	endpoint := fmt.Sprintf("%s/me/messages/%s/move", baseURL, messageID)

	body := map[string]string{
		"destinationId": destinationFolderID,
	}

	var result Message
	if err := c.post(ctx, endpoint, body, &result); err != nil {
		return nil, err
	}

	return &result, nil
}

// UpdateCategories updates the categories on a message
func (c *Client) UpdateCategories(ctx context.Context, messageID string, categories []string) error {
	endpoint := fmt.Sprintf("%s/me/messages/%s", baseURL, messageID)

	body := map[string]interface{}{
		"categories": categories,
	}

	return c.patch(ctx, endpoint, body)
}

// Helper methods for HTTP requests
func (c *Client) get(ctx context.Context, endpoint string, result interface{}) error {
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bearer "+c.accessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API request failed with status %d: %s", resp.StatusCode, string(body))
	}

	return json.NewDecoder(resp.Body).Decode(result)
}

func (c *Client) post(ctx context.Context, endpoint string, body interface{}, result interface{}) error {
	return c.doJSON(ctx, "POST", endpoint, body, result)
}

func (c *Client) patch(ctx context.Context, endpoint string, body interface{}) error {
	return c.doJSON(ctx, "PATCH", endpoint, body, nil)
}

func (c *Client) doJSON(ctx context.Context, method, endpoint string, body interface{}, result interface{}) error {
	var bodyReader io.Reader
	if body != nil {
		jsonData, err := json.Marshal(body)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(jsonData)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, bodyReader)
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bearer "+c.accessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API request failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	if result != nil {
		return json.NewDecoder(resp.Body).Decode(result)
	}

	return nil
}
