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
	p, err := s.CreateProject(ctx, domain.Project{UserID: u.ID, Name: "Tool", Language: "Go", RepoURL: "https://github.com/a/b", AuthorDescription: "Описание автора", Topics: "golang,telegram-bot", Stars: "12", WantsContributors: true})
	if err != nil {
		t.Fatal(err)
	}
	items, err := s.ListProjects(ctx, 0, true, 5, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != p.ID {
		t.Fatalf("unexpected projects: %+v", items)
	}
	if items[0].AuthorDescription != "Описание автора" || items[0].Topics != "golang,telegram-bot" {
		t.Fatalf("project metadata was not saved: %+v", items[0])
	}
	if err = s.SetChannelPublication(ctx, p.ID, -10099, 77); err != nil {
		t.Fatal(err)
	}
	stored, err := s.ProjectForUser(ctx, p.ID, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ChannelChatID != -10099 || stored.ChannelMessageID != 77 {
		t.Fatalf("channel publication not saved: %+v", stored)
	}
	items, err = s.ListProjects(ctx, 10, false, 5, 0)
	if err != nil || len(items) != 1 {
		t.Fatalf("star filter missed project: %+v %v", items, err)
	}
	items, err = s.ListProjects(ctx, 50, false, 5, 0)
	if err != nil || len(items) != 0 {
		t.Fatalf("star filter included project: %+v %v", items, err)
	}
	if err := s.SetProjectStatus(ctx, p.ID, domain.StatusHidden); err != nil {
		t.Fatal(err)
	}
	items, err = s.ListProjects(ctx, 0, false, 5, 0)
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

func TestOwnerCanUpdateAndCloseProject(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	owner, err := s.UpsertUser(ctx, domain.User{TelegramID: 1})
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.UpsertUser(ctx, domain.User{TelegramID: 2})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.CreateProject(ctx, domain.Project{UserID: owner.ID, Name: "One", Language: "Go", RepoURL: "https://github.com/a/b"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.UpdateProjectDescription(ctx, p.ID, other.ID, "чужое описание"); err == nil {
		t.Fatal("another user updated project")
	}
	if err = s.UpdateProjectDescription(ctx, p.ID, owner.ID, "новое описание"); err != nil {
		t.Fatal(err)
	}
	if err = s.CloseProject(ctx, p.ID, owner.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.ProjectForUser(ctx, p.ID, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.StatusClosed || got.AuthorDescription != "новое описание" {
		t.Fatalf("unexpected project: %+v", got)
	}
	if err = s.UpdateProjectDescription(ctx, p.ID, owner.ID, "после закрытия"); err == nil {
		t.Fatal("closed project was updated")
	}
}
