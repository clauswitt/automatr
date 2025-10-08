package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const (
	authURL  = "https://login.microsoftonline.com/%s/oauth2/v2.0/authorize"
	tokenURL = "https://login.microsoftonline.com/%s/oauth2/v2.0/token"
)

type Manager struct {
	clientID     string
	clientSecret string
	tenantID     string
	redirectURI  string
	scopes       []string
	tokenPath    string
}

type TokenStore struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	TokenType    string    `json:"token_type"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func NewManager(clientID, clientSecret, tenantID, redirectURI string, scopes []string) *Manager {
	homeDir, _ := os.UserHomeDir()
	tokenPath := filepath.Join(homeDir, ".automatr", "token.json")

	return &Manager{
		clientID:     clientID,
		clientSecret: clientSecret,
		tenantID:     tenantID,
		redirectURI:  redirectURI,
		scopes:       scopes,
		tokenPath:    tokenPath,
	}
}

// GetToken returns a valid access token, refreshing if necessary
func (m *Manager) GetToken(ctx context.Context) (*TokenStore, error) {
	// Try to load existing token
	token, err := m.loadToken()
	if err == nil {
		// Check if token is still valid
		if time.Now().Before(token.ExpiresAt.Add(-5 * time.Minute)) {
			return token, nil
		}

		// Try to refresh
		if token.RefreshToken != "" {
			newToken, err := m.refreshToken(ctx, token.RefreshToken)
			if err == nil {
				return newToken, nil
			}
			fmt.Printf("Token refresh failed: %v\n", err)
		}
	}

	// Need to authenticate
	return m.authenticate(ctx)
}

func (m *Manager) authenticate(ctx context.Context) (*TokenStore, error) {
	// Generate PKCE challenge
	verifier := generateCodeVerifier()
	challenge := generateCodeChallenge(verifier)

	// Build auth URL
	authURLStr := fmt.Sprintf(authURL, m.tenantID)
	params := url.Values{
		"client_id":             {m.clientID},
		"response_type":         {"code"},
		"redirect_uri":          {m.redirectURI},
		"scope":                 {scopesToString(m.scopes)},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}

	fullAuthURL := authURLStr + "?" + params.Encode()

	// Start local server to receive callback
	codeChan := make(chan string, 1)
	errChan := make(chan error, 1)

	server := &http.Server{Addr: ":8383"}
	http.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		if code == "" {
			errChan <- fmt.Errorf("no authorization code received")
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("Authorization failed"))
			return
		}

		codeChan <- code
		w.Write([]byte("✓ Authorization successful! You can close this window."))
	})

	go func() {
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			errChan <- err
		}
	}()

	fmt.Println("Opening browser for authentication...")
	fmt.Printf("If browser doesn't open, visit: %s\n", fullAuthURL)

	// Try to open browser
	openBrowser(fullAuthURL)

	// Wait for callback
	var code string
	select {
	case code = <-codeChan:
		server.Shutdown(ctx)
	case err := <-errChan:
		return nil, err
	case <-time.After(5 * time.Minute):
		server.Shutdown(ctx)
		return nil, fmt.Errorf("authentication timeout")
	}

	// Exchange code for token
	return m.exchangeCode(ctx, code, verifier)
}

func (m *Manager) exchangeCode(ctx context.Context, code, verifier string) (*TokenStore, error) {
	tokenURLStr := fmt.Sprintf(tokenURL, m.tenantID)

	data := url.Values{
		"client_id":    {m.clientID},
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {m.redirectURI},
	}

	// Use client_secret if available (confidential client), otherwise PKCE (public client)
	if m.clientSecret != "" {
		data.Set("client_secret", m.clientSecret)
	} else {
		data.Set("code_verifier", verifier)
	}

	resp, err := http.PostForm(tokenURLStr, data)
	if err != nil {
		return nil, fmt.Errorf("token exchange failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("token exchange failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
		Error        string `json:"error"`
		ErrorDesc    string `json:"error_description"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to parse token response: %w", err)
	}

	if result.Error != "" {
		return nil, fmt.Errorf("token exchange error: %s - %s", result.Error, result.ErrorDesc)
	}

	if result.AccessToken == "" {
		return nil, fmt.Errorf("no access token in response")
	}

	token := &TokenStore{
		AccessToken:  result.AccessToken,
		RefreshToken: result.RefreshToken,
		TokenType:    result.TokenType,
		ExpiresAt:    time.Now().Add(time.Duration(result.ExpiresIn) * time.Second),
	}

	// Save token
	if err := m.saveToken(token); err != nil {
		return nil, fmt.Errorf("failed to save token: %w", err)
	}

	return token, nil
}

func (m *Manager) refreshToken(ctx context.Context, refreshToken string) (*TokenStore, error) {
	tokenURLStr := fmt.Sprintf(tokenURL, m.tenantID)

	data := url.Values{
		"client_id":     {m.clientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"scope":         {scopesToString(m.scopes)},
	}

	if m.clientSecret != "" {
		data.Set("client_secret", m.clientSecret)
	}

	resp, err := http.PostForm(tokenURLStr, data)
	if err != nil {
		return nil, fmt.Errorf("token refresh failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("token refresh failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int    `json:"expires_in"`
		Error        string `json:"error"`
		ErrorDesc    string `json:"error_description"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to parse token response: %w", err)
	}

	if result.Error != "" {
		return nil, fmt.Errorf("token refresh error: %s - %s", result.Error, result.ErrorDesc)
	}

	token := &TokenStore{
		AccessToken:  result.AccessToken,
		RefreshToken: result.RefreshToken,
		TokenType:    result.TokenType,
		ExpiresAt:    time.Now().Add(time.Duration(result.ExpiresIn) * time.Second),
	}

	if err := m.saveToken(token); err != nil {
		return nil, fmt.Errorf("failed to save token: %w", err)
	}

	return token, nil
}

func (m *Manager) loadToken() (*TokenStore, error) {
	data, err := os.ReadFile(m.tokenPath)
	if err != nil {
		return nil, err
	}

	var token TokenStore
	if err := json.Unmarshal(data, &token); err != nil {
		return nil, err
	}

	return &token, nil
}

func (m *Manager) saveToken(token *TokenStore) error {
	dir := filepath.Dir(m.tokenPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	data, err := json.MarshalIndent(token, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(m.tokenPath, data, 0600)
}

// PKCE helpers
func generateCodeVerifier() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func generateCodeChallenge(verifier string) string {
	h := sha256.New()
	h.Write([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

func scopesToString(scopes []string) string {
	result := ""
	for i, scope := range scopes {
		if i > 0 {
			result += " "
		}
		result += scope
	}
	return result
}

func openBrowser(url string) error {
	// macOS-specific
	return exec.Command("open", url).Start()
}
