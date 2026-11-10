package t3

import (
	"bytes"
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
	"strings"
	"sync"
	"time"
)

type Credentials struct {
	Origin       string `json:"origin"`
	ClientID     string `json:"client_id"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAt    int64  `json:"expires_at"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

var credentialsMu sync.Mutex

func ValidateOrigin(origin string) (string, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("T3 URL must be an HTTP origin, for example http://127.0.0.1:3773")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")) {
		return "", fmt.Errorf("T3 requires HTTPS except on loopback")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func randomToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func post(ctx context.Context, endpoint, contentType string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	// OAuth redirects contain authorization codes; never follow or include them in errors.
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("T3 OAuth request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("T3 OAuth endpoint returned HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return fmt.Errorf("invalid T3 OAuth response: %w", err)
	}
	return nil
}

func postJSON(ctx context.Context, endpoint string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return post(ctx, endpoint, "application/json", body, out)
}

// Setup obtains credentials through T3's scoped, short-lived headless pairing flow.
func Setup(ctx context.Context, origin, path, command string) error {
	var err error
	origin, err = ValidateOrigin(origin)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, command, "auth", "pairing", "create", "--scope", "orchestration:read", "--scope", "orchestration:operate", "--scope", "settings:write", "--scope", "source-control:write", "--label", "Threads", "--ttl", "5m", "--json")
	pairingJSON, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("T3 pairing failed; check that t3 is installed and signed in")
	}
	var pairing struct {
		Credential string `json:"credential"`
	}
	if err := json.Unmarshal(pairingJSON, &pairing); err != nil || pairing.Credential == "" {
		return fmt.Errorf("T3 pairing response has no credential")
	}
	redirect := "http://127.0.0.1:49173/callback"
	var registration struct {
		ClientID string `json:"client_id"`
	}
	if err := postJSON(ctx, origin+"/oauth/mcp/register", map[string]any{"client_name": "Threads", "redirect_uris": []string{redirect}}, &registration); err != nil {
		return err
	}
	if registration.ClientID == "" {
		return fmt.Errorf("T3 registration returned no client id")
	}
	verifier, state := randomToken(), randomToken()
	challenge := sha256.Sum256([]byte(verifier))
	var decision struct {
		RedirectTo string `json:"redirectTo"`
	}
	err = postJSON(ctx, origin+"/oauth/mcp/decision", map[string]any{
		"authorization": map[string]string{"response_type": "code", "client_id": registration.ClientID, "redirect_uri": redirect, "code_challenge": base64.RawURLEncoding.EncodeToString(challenge[:]), "code_challenge_method": "S256", "state": state, "resource": origin + "/mcp"},
		"decision":      map[string]string{"_tag": "pairing-code", "access": "full-access", "code": pairing.Credential},
	}, &decision)
	if err != nil {
		return err
	}
	target, err := url.Parse(decision.RedirectTo)
	if err != nil || target.Scheme+"://"+target.Host+target.Path != redirect || target.Query().Get("state") != state || target.Query().Get("code") == "" {
		return fmt.Errorf("T3 authorization redirect failed state/code validation")
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {target.Query().Get("code")}, "redirect_uri": {redirect}, "client_id": {registration.ClientID}, "code_verifier": {verifier}, "resource": {origin + "/mcp"}}
	var tokens tokenResponse
	if err := post(ctx, origin+"/oauth/mcp/token", "application/x-www-form-urlencoded", []byte(form.Encode()), &tokens); err != nil {
		return err
	}
	if tokens.AccessToken == "" {
		return fmt.Errorf("T3 returned no access token")
	}
	return saveCredentials(path, Credentials{Origin: origin, ClientID: registration.ClientID, AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken, ExpiresAt: time.Now().Unix() + tokens.ExpiresIn})
}

func saveCredentials(path string, creds Credentials) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".t3-credentials-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	data, err := json.MarshalIndent(creds, "", "  ")
	if err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(file.Name(), path)
}

func accessToken(ctx context.Context, origin, path string) (string, error) {
	credentialsMu.Lock()
	defer credentialsMu.Unlock()
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("read T3 credentials: %w (run -t3-setup)", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return "", fmt.Errorf("T3 credentials must be a regular file accessible only to its owner (chmod 600)")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var creds Credentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return "", fmt.Errorf("invalid T3 credential file")
	}
	if creds.Origin != origin || creds.ClientID == "" || creds.AccessToken == "" {
		return "", fmt.Errorf("T3 credentials do not match configured origin; run -t3-setup")
	}
	if creds.ExpiresAt > 0 && time.Now().Unix()+60 >= creds.ExpiresAt {
		if creds.RefreshToken == "" {
			return "", fmt.Errorf("T3 credentials expired; run -t3-setup")
		}
		form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {creds.RefreshToken}, "client_id": {creds.ClientID}, "resource": {origin + "/mcp"}}
		var tokens tokenResponse
		if err := post(ctx, origin+"/oauth/mcp/token", "application/x-www-form-urlencoded", []byte(form.Encode()), &tokens); err != nil {
			return "", err
		}
		if tokens.AccessToken == "" {
			return "", fmt.Errorf("T3 token refresh returned no access token")
		}
		creds.AccessToken = tokens.AccessToken
		if tokens.RefreshToken != "" {
			creds.RefreshToken = tokens.RefreshToken
		}
		creds.ExpiresAt = time.Now().Unix() + tokens.ExpiresIn
		if err := saveCredentials(path, creds); err != nil {
			return "", err
		}
	}
	return creds.AccessToken, nil
}
