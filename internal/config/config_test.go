package config

import "testing"

func TestLoadRequiresProjectGroupsJSON(t *testing.T) {
	t.Setenv("BOT_TOKEN", "test-token")
	t.Setenv("PROJECT_GROUPS_JSON", "")
	if _, err := Load(); err == nil {
		t.Fatal("empty project group settings were accepted")
	}
}

func TestLoadProjectGroupsJSON(t *testing.T) {
	t.Setenv("BOT_TOKEN", "test-token")
	t.Setenv("PROJECT_GROUPS_JSON", `[{"language":"Go","chat_id":-1001,"chat_username":"@go","thread_id":5},{"language":"Python","chat_id":-1002,"thread_id":7}]`)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.ProjectTargets) != 2 {
		t.Fatalf("unexpected targets: %+v", cfg.ProjectTargets)
	}
	target, ok := cfg.TargetForLanguage("Python")
	if !ok || target.ChatID != -1002 || target.ThreadID != 7 {
		t.Fatalf("unexpected target: %+v", target)
	}
}
