package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaultsAndTokenEnv(t *testing.T) {
	t.Setenv("THREADS_TOKEN_TEST", "secret")
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{
		"scopes":[{"id":"s1","threads":{"base_url":"https://threads","token_env":"THREADS_TOKEN_TEST"},"runner":{}}]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabasePath == "" || cfg.Scopes[0].Runner.Command != "codex" {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
	token, err := cfg.Scopes[0].Token()
	if err != nil || token != "secret" {
		t.Fatalf("token = %q, err=%v", token, err)
	}
}

func TestLoadClaudeCodeDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{
		"scopes":[{"id":"s1","threads":{"base_url":"https://threads","token":"secret"},"runner":{"type":"claude-code"}}]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Scopes[0].Runner.Command != "claude" || !contains(cfg.Scopes[0].Runner.Args, "stream-json") {
		t.Fatalf("claude defaults not applied: %+v", cfg.Scopes[0].Runner)
	}
}

func TestScopeMatchesDefaultsToNone(t *testing.T) {
	s := Scope{}
	if s.Matches("c1", "t1") || s.Matches("c2", "") {
		t.Fatal("empty match config should not match delivered events")
	}
}

func TestScopeMatchesWildcardAll(t *testing.T) {
	s := Scope{Match: MatchConfig{ChannelIDs: []string{"*"}, ThreadIDs: []string{"*"}}}
	if !s.Matches("c1", "t1") || !s.Matches("c2", "") {
		t.Fatal("wildcard match config should match all delivered events")
	}
}

func TestScopeMatchesAllowlists(t *testing.T) {
	s := Scope{Match: MatchConfig{ChannelIDs: []string{"c1"}, ThreadIDs: []string{"t1"}}}
	if !s.Matches("c1", "t1") {
		t.Fatal("expected configured channel/thread to match")
	}
	if s.Matches("c2", "t1") {
		t.Fatal("unexpected channel match")
	}
	if s.Matches("c1", "t2") {
		t.Fatal("unexpected thread match")
	}
}

func TestLoadPiDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{
		"scopes":[{"id":"s1","threads":{"base_url":"https://threads","token":"secret"},"runner":{"type":"pi"}}]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Scopes[0].Runner.Command != "pi" || !contains(cfg.Scopes[0].Runner.Args, "json") {
		t.Fatalf("pi defaults not applied: %+v", cfg.Scopes[0].Runner)
	}
}

func TestLoadAutoTitleRequiresStructuredOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{
		"scopes":[{"id":"s1","threads":{"base_url":"https://threads","token":"secret"},"runner":{"auto_title":true}}]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected auto_title configuration error")
	}
}

func TestLoadAutoTitleWithStructuredOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{
		"scopes":[{"id":"s1","threads":{"base_url":"https://threads","token":"secret"},"runner":{"structured":true,"auto_title":true}}]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Scopes[0].Runner.AutoTitle {
		t.Fatalf("auto_title not loaded: %+v", cfg.Scopes[0].Runner)
	}
}

func TestLoadT3RequiresExplicitTargetAndDefaultsToApprovals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{"scopes":[{"id":"t3","threads":{"base_url":"http://localhost/api","token_env":"TOKEN"},"runner":{"type":"t3","t3":{"base_url":"http://127.0.0.1:3773","credentials_file":".local/token.json","project_id":"project","model_selection":{"instanceId":"codex","model":"gpt-6.1-sol"}}}}]}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	target := cfg.Scopes[0].Runner.T3
	if target.RuntimeMode != "approval-required" || target.InteractionMode != "default" {
		t.Fatalf("unsafe defaults: %+v", target)
	}
	data = `{"scopes":[{"id":"t3","threads":{"base_url":"http://localhost/api","token_env":"TOKEN"},"runner":{"type":"t3","t3":{"base_url":"http://127.0.0.1:3773"}}}]}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("accepted implicit T3 target/model")
	}
}
