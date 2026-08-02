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
	byUsername, err := s.UserByUsername(ctx, "@GOPHER")
	if err != nil || byUsername.TelegramID != 42 {
		t.Fatalf("username lookup failed: %+v %v", byUsername, err)
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

func TestNetworkAdministration(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	g := domain.NetworkGroup{Name: "Gophers", Language: "Go", ChatID: -1001, ChatUsername: "@go", ThreadID: 5}
	if err = s.UpsertNetworkGroup(ctx, g); err != nil {
		t.Fatal(err)
	}
	groups, err := s.NetworkGroups(ctx)
	if err != nil || len(groups) != 1 {
		t.Fatalf("unexpected groups: %+v %v", groups, err)
	}
	g.Language = "Golang"
	if err = s.UpsertNetworkGroup(ctx, g); err != nil {
		t.Fatal(err)
	}
	groups, err = s.NetworkGroups(ctx)
	if err != nil || len(groups) != 1 || groups[0].Language != "Golang" {
		t.Fatalf("group route was not replaced: %+v %v", groups, err)
	}
	enabled, err := s.ToggleAntiSpam(ctx, -1001)
	if err != nil || !enabled {
		t.Fatalf("antispam not enabled: %v %v", enabled, err)
	}
	if err = s.SetAntiSpamLimit(ctx, -1001, 10); err != nil {
		t.Fatal(err)
	}
	if err = s.SetAntiSpamWindow(ctx, -1001, 30); err != nil {
		t.Fatal(err)
	}
	if err = s.SetAntiSpamAction(ctx, -1001, "delete"); err != nil {
		t.Fatal(err)
	}
	configured, err := s.NetworkGroupByChat(ctx, -1001)
	if err != nil || configured.SpamLimit != 10 || configured.SpamWindow != 30 || configured.SpamAction != "delete" {
		t.Fatalf("unexpected antispam settings: %+v %v", configured, err)
	}
	u, err := s.UpsertUser(ctx, domain.User{TelegramID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetRole(ctx, 7, "moderator"); err != nil {
		t.Fatal(err)
	}
	if err = s.AddWarn(ctx, 7); err != nil {
		t.Fatal(err)
	}
	if err = s.SetTags(ctx, 7, "trusted,go"); err != nil {
		t.Fatal(err)
	}
	u, err = s.UserByTelegramID(ctx, 7)
	if err != nil || u.Role != "moderator" || u.Warns != 1 || u.Tags != "trusted,go" {
		t.Fatalf("unexpected user: %+v %v", u, err)
	}
	if err = s.SaveCustomCommand(ctx, "hello", "Привет, {name}", 7); err != nil {
		t.Fatal(err)
	}
	cmd, err := s.CustomCommand(ctx, "hello")
	if err != nil || cmd.Response != "Привет, {name}" {
		t.Fatalf("unexpected command: %+v %v", cmd, err)
	}
}

func TestRegisterAndSanctionGroup(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	registered := domain.RegisteredGroup{Name: "Go community", ChatID: -1001, ChatUsername: "@go"}
	if err = s.RegisterGroup(ctx, registered, 7); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RegisteredGroupByChat(ctx, -1001); err != nil {
		t.Fatal(err)
	}
	registeredList, listErr := s.RegisteredGroups(ctx)
	if listErr != nil || len(registeredList) != 1 || registeredList[0].Name != "Go community" {
		t.Fatalf("unexpected registered groups: %+v %v", registeredList, listErr)
	}
	if err = s.UpsertNetworkGroup(ctx, domain.NetworkGroup{Name: "Projects", Language: "Go", ChatID: -1001, ThreadID: 5}); err != nil {
		t.Fatal(err)
	}
	if err = s.SanctionGroup(ctx, -1001); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RegisteredGroupByChat(ctx, -1001); err == nil {
		t.Fatal("sanctioned group remains registered")
	}
	if groups, err := s.NetworkGroups(ctx); err != nil || len(groups) != 0 {
		t.Fatalf("route remains after sanction: %+v %v", groups, err)
	}
}
