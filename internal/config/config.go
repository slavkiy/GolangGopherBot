package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

type ProjectTarget struct {
	Language     string `json:"language"`
	ChatID       int64  `json:"chat_id"`
	ChatUsername string `json:"chat_username"`
	ThreadID     int    `json:"thread_id"`
}

type PublishTarget struct {
	ChatID   int64
	Username string
}

type Config struct {
	BotToken        string
	DatabasePath    string
	AdminIDs        map[int64]struct{}
	GitHubToken     string
	ProjectTargets  []ProjectTarget
	ProjectsChannel PublishTarget
	NetworkOwnerID  int64
}

func Load() (Config, error) {
	_ = godotenv.Load()
	cfg := Config{
		BotToken: os.Getenv("BOT_TOKEN"), DatabasePath: valueOr("DATABASE_PATH", "data/bot.db"),
		AdminIDs: parseIDs(os.Getenv("ADMIN_IDS")), GitHubToken: os.Getenv("GITHUB_TOKEN"),
	}
	cfg.ProjectsChannel.ChatID, _ = strconv.ParseInt(os.Getenv("PROJECTS_CHANNEL_ID"), 10, 64)
	cfg.NetworkOwnerID, _ = strconv.ParseInt(os.Getenv("NETWORK_OWNER_ID"), 10, 64)
	cfg.ProjectsChannel.Username = os.Getenv("PROJECTS_CHANNEL_USERNAME")
	if cfg.BotToken == "" {
		return Config{}, fmt.Errorf("BOT_TOKEN is not set")
	}
	if raw := os.Getenv("PROJECT_GROUPS_JSON"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &cfg.ProjectTargets); err != nil {
			return Config{}, fmt.Errorf("PROJECT_GROUPS_JSON: %w", err)
		}
	}
	// PROJECT_GROUPS_JSON используется только для первоначального наполнения БД.
	seen := map[string]bool{}
	for i := range cfg.ProjectTargets {
		t := &cfg.ProjectTargets[i]
		t.Language = strings.TrimSpace(t.Language)
		if t.ThreadID == 0 {
			t.ThreadID = 5
		}
		if t.Language == "" || seen[t.Language] || t.ChatID == 0 && t.ChatUsername == "" {
			return Config{}, fmt.Errorf("invalid or duplicate project target %q", t.Language)
		}
		seen[t.Language] = true
	}
	return cfg, nil
}

func (c Config) TargetForLanguage(language string) (ProjectTarget, bool) {
	for _, target := range c.ProjectTargets {
		if target.Language == language {
			return target, true
		}
	}
	return ProjectTarget{}, false
}

func valueOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
func parseIDs(raw string) map[int64]struct{} {
	result := make(map[int64]struct{})
	for _, part := range strings.Split(raw, ",") {
		if id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64); err == nil {
			result[id] = struct{}{}
		}
	}
	return result
}
