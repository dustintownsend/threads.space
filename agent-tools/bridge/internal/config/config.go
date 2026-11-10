package config

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/danielcorin/threads.space/agent-tools/bridge/internal/t3"
)

type Config struct {
	DatabasePath string  `json:"database_path"`
	Scopes       []Scope `json:"scopes"`
}

type Scope struct {
	ID      string        `json:"id"`
	Threads ThreadsConfig `json:"threads"`
	Match   MatchConfig   `json:"match"`
	Runner  RunnerConfig  `json:"runner"`
	Safety  SafetyConfig  `json:"safety"`
}

type ThreadsConfig struct {
	BaseURL  string `json:"base_url"`
	Token    string `json:"token"`
	TokenEnv string `json:"token_env"`
	UserID   string `json:"user_id"`
	Since    string `json:"since"`
}

type MatchConfig struct {
	// Allowlists are deny-by-default: use "*" to match all delivered events.
	ChannelIDs []string `json:"channel_ids"`
	ThreadIDs  []string `json:"thread_ids"`
}

type T3Config struct {
	BaseURL         string         `json:"base_url"`
	CredentialsFile string         `json:"credentials_file"`
	ProjectID       string         `json:"project_id"`
	ModelSelection  ModelSelection `json:"model_selection"`
	RuntimeMode     string         `json:"runtime_mode"`
	InteractionMode string         `json:"interaction_mode"`
}

type ModelSelection struct {
	InstanceID string         `json:"instanceId"`
	Model      string         `json:"model"`
	Options    map[string]any `json:"options,omitempty"`
}

type RunnerConfig struct {
	T3         T3Config `json:"t3"`
	Type       string   `json:"type"`
	Command    string   `json:"command"`
	Args       []string `json:"args"`
	WorkingDir string   `json:"working_dir"`
	// Structured enables JSON final-output handling: {"content":"...","reactions":[{"message_id":"...","emoji":"👍"}]}.
	Structured bool `json:"structured"`
	// AutoTitle asks the first root-message inference for a title and requires structured output.
	AutoTitle bool `json:"auto_title"`
}

type SafetyConfig struct {
	Mode         string   `json:"mode"`
	AllowedTools []string `json:"allowed_tools"`
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	if cfg.DatabasePath == "" {
		cfg.DatabasePath = "threads-agent-bridge.db"
	}
	for i := range cfg.Scopes {
		if cfg.Scopes[i].ID == "" {
			return Config{}, fmt.Errorf("scopes[%d].id is required", i)
		}
		if cfg.Scopes[i].Threads.BaseURL == "" {
			return Config{}, fmt.Errorf("scope %q threads.base_url is required", cfg.Scopes[i].ID)
		}
		if cfg.Scopes[i].Threads.Token == "" && cfg.Scopes[i].Threads.TokenEnv == "" {
			return Config{}, fmt.Errorf("scope %q requires threads.token or threads.token_env", cfg.Scopes[i].ID)
		}
		if cfg.Scopes[i].Runner.Type == "" {
			cfg.Scopes[i].Runner.Type = "codex"
		}
		if cfg.Scopes[i].Runner.AutoTitle && !cfg.Scopes[i].Runner.Structured {
			return Config{}, fmt.Errorf("scope %q runner.auto_title requires runner.structured", cfg.Scopes[i].ID)
		}
		switch cfg.Scopes[i].Runner.Type {
		case "t3":
			t := &cfg.Scopes[i].Runner.T3
			origin, err := t3.ValidateOrigin(t.BaseURL)
			if err != nil {
				return Config{}, err
			}
			t.BaseURL = origin
			if t.CredentialsFile == "" || t.ProjectID == "" || t.ModelSelection.InstanceID == "" || t.ModelSelection.Model == "" {
				return Config{}, fmt.Errorf("scope %q T3 requires credentials_file, project_id and model_selection (instanceId/model)", cfg.Scopes[i].ID)
			}
			if t.RuntimeMode == "" {
				t.RuntimeMode = "approval-required"
			}
			if t.InteractionMode == "" {
				t.InteractionMode = "default"
			}
			switch t.RuntimeMode {
			case "approval-required", "auto-accept-edits", "auto", "full-access":
			default:
				return Config{}, fmt.Errorf("invalid T3 runtime_mode %q", t.RuntimeMode)
			}
			if t.InteractionMode != "default" && t.InteractionMode != "plan" {
				return Config{}, fmt.Errorf("invalid T3 interaction_mode %q", t.InteractionMode)
			}
		case "claude-code":
			if cfg.Scopes[i].Runner.Command == "" {
				cfg.Scopes[i].Runner.Command = "claude"
			}
			if len(cfg.Scopes[i].Runner.Args) == 0 {
				cfg.Scopes[i].Runner.Args = []string{"-p", "--verbose", "--output-format", "stream-json"}
			}
		case "pi":
			if cfg.Scopes[i].Runner.Command == "" {
				cfg.Scopes[i].Runner.Command = "pi"
			}
			if len(cfg.Scopes[i].Runner.Args) == 0 {
				cfg.Scopes[i].Runner.Args = []string{"--mode", "json", "--print"}
			}
		default:
			if cfg.Scopes[i].Runner.Command == "" {
				cfg.Scopes[i].Runner.Command = "codex"
			}
			if len(cfg.Scopes[i].Runner.Args) == 0 && cfg.Scopes[i].Runner.Type == "codex" {
				cfg.Scopes[i].Runner.Args = []string{"exec", "--json"}
			}
		}
	}
	return cfg, nil
}

func (s Scope) Token() (string, error) {
	if s.Threads.Token != "" {
		return s.Threads.Token, nil
	}
	if s.Threads.TokenEnv == "" {
		return "", fmt.Errorf("scope %q has no token source", s.ID)
	}
	value := os.Getenv(s.Threads.TokenEnv)
	if value == "" {
		return "", fmt.Errorf("scope %q token env %s is empty", s.ID, s.Threads.TokenEnv)
	}
	return value, nil
}

func (s Scope) Matches(channelID, threadID string) bool {
	return matchesAllowlist(s.Match.ChannelIDs, channelID) && matchesAllowlist(s.Match.ThreadIDs, threadID)
}

func matchesAllowlist(values []string, needle string) bool {
	if len(values) == 0 {
		return false
	}
	for _, value := range values {
		if value == "*" || value == needle {
			return true
		}
	}
	return false
}

func contains(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}
