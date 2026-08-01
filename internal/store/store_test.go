package store

import (
	"context"
	"path/filepath"
	"testing"

	"golanggopherbot/internal/domain"
)

func TestUserAndProjectFlow(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	u, err := s.UpsertUser(ctx, domain.User{TelegramID: 42, Username: "gopher", FirstName: "Go"})
	if err != nil {
		t.Fatal(err)
	}
	if u.ID == 0 || u.Username != "gopher" {
		t.Fatalf("unexpected user: %+v", u)
	}
	p, err := s.CreateProject(ctx, domain.Project{UserID: u.ID, Name: "Tool", Language: "Go", RepoURL: "https://github.com/a/b", WantsContributors: true})
	if err != nil {
		t.Fatal(err)
	}
	items, err := s.ListProjects(ctx, "Go", true, 5, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != p.ID {
		t.Fatalf("unexpected projects: %+v", items)
	}
	if err := s.SetProjectStatus(ctx, p.ID, domain.StatusHidden); err != nil {
		t.Fatal(err)
	}
	items, err = s.ListProjects(ctx, "", false, 5, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("hidden project is visible: %+v", items)
	}
}

func TestRepositoryURLIsUnique(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	u, err := s.UpsertUser(ctx, domain.User{TelegramID: 1})
	if err != nil {
		t.Fatal(err)
	}
	p := domain.Project{UserID: u.ID, Name: "One", Language: "Go", RepoURL: "https://github.com/a/b"}
	if _, err = s.CreateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateProject(ctx, p); err == nil {
		t.Fatal("duplicate repository accepted")
	}
}
