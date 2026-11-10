package t3

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMCPStreamableHTTPAndPrivateCredentials(t *testing.T) {
	initialized := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing bearer credential")
			w.WriteHeader(401)
			return
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(200)
			return
		}
		var request struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		switch request.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "session")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-03-26"}}`, request.ID)
		case "notifications/initialized":
			initialized = true
			w.WriteHeader(202)
		case "tools/call":
			if !initialized || r.Header.Get("Mcp-Session-Id") != "session" {
				t.Error("session not initialized")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\nevent: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"structuredContent\":{\"projects\":[]}}}\n\n", request.ID)
		}
	}))
	defer server.Close()
	file := filepath.Join(t.TempDir(), "credentials.json")
	if err := saveCredentials(file, Credentials{Origin: server.URL, ClientID: "client", AccessToken: "test-token", ExpiresAt: time.Now().Unix() + 3600}); err != nil {
		t.Fatal(err)
	}
	client, err := New(context.Background(), server.URL, file)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	result, err := client.Check(context.Background())
	if err != nil || string(result) != `{"projects":[]}` {
		t.Fatalf("result=%s err=%v", result, err)
	}
	if err := os.Chmod(file, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := accessToken(context.Background(), server.URL, file); err == nil {
		t.Fatal("accepted readable credentials")
	}
	if err := os.Chmod(file, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestTokenRefreshPersistsRotatedCredentials(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Form.Get("refresh_token") != "old-refresh" || r.Form.Get("client_id") != "client" {
			t.Error("wrong refresh request")
		}
		fmt.Fprint(w, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`)
	}))
	defer server.Close()
	file := filepath.Join(t.TempDir(), "credentials.json")
	if err := saveCredentials(file, Credentials{Origin: server.URL, ClientID: "client", AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: 1}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		token, err := accessToken(context.Background(), server.URL, file)
		if err != nil || token != "new-access" {
			t.Fatalf("token=%q err=%v", token, err)
		}
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var creds Credentials
	if err := json.Unmarshal(data, &creds); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || creds.RefreshToken != "new-refresh" {
		t.Fatalf("calls=%d credentials not rotated", calls)
	}
}

func TestValidateOrigin(t *testing.T) {
	for _, origin := range []string{"http://example.com", "http://user:secret@localhost:3773", "http://localhost:3773/mcp", "file:///tmp/t3"} {
		if _, err := ValidateOrigin(origin); err == nil {
			t.Fatalf("accepted %s", origin)
		}
	}
}
