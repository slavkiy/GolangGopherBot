package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

type Config struct {
	BotToken             string
	DatabasePath         string
	ProjectsChatID       int64
	ProjectsChatUsername string
	ProjectsThreadID     int
	AdminIDs             map[int64]struct{}
	GitHubToken          string
}

func Load() (Config, error) {
	_ = godotenv.Load()
	cfg := Config{
		BotToken: os.Getenv("BOT_TOKEN"), DatabasePath: valueOr("DATABASE_PATH", "data/bot.db"),
		ProjectsChatUsername: os.Getenv("PROJECTS_CHAT_USERNAME"), ProjectsThreadID: intValue("PROJECTS_THREAD_ID", 5),
		AdminIDs: parseIDs(os.Getenv("ADMIN_IDS")), GitHubToken: os.Getenv("GITHUB_TOKEN"),
	}
	cfg.ProjectsChatID, _ = strconv.ParseInt(os.Getenv("PROJECTS_CHAT_ID"), 10, 64)
	if cfg.BotToken == "" {
		return Config{}, fmt.Errorf("BOT_TOKEN is not set")
	}
	if cfg.ProjectsChatID == 0 && cfg.ProjectsChatUsername == "" {
		return Config{}, fmt.Errorf("PROJECTS_CHAT_ID or PROJECTS_CHAT_USERNAME must be set")
	}
	return cfg, nil
}

func valueOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
func intValue(key string, fallback int) int {
	v, err := strconv.Atoi(os.Getenv(key))
	if err != nil {
		return fallback
	}
	return v
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
