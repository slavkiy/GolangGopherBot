package bot

import (
	"strings"
	"testing"

	"golanggopherbot/internal/config"
	"golanggopherbot/internal/domain"
	gh "golanggopherbot/internal/github"
)

func TestFormatProjectEscapesHTML(t *testing.T) {
	text := formatProject(domain.Project{Name: "<tool>", Language: "Go", Stars: "2", Description: "A & B", RepoURL: "https://github.com/a/b", WantsContributors: true}, "@author")
	for _, want := range []string{"&lt;tool&gt;", "A &amp; B", "нужны ✅", "@author"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in %q", want, text)
		}
	}
}

func TestPublishedProjectHasAttributionAndTopics(t *testing.T) {
	p := domain.Project{Name: "Tool", RepoURL: "https://github.com/a/b", Topics: "golang,open-source,cli tools"}
	text := formatPublishedProject(p, "@author", "@GolangGopher")
	for _, want := range []string{"#golang", "#open_source", "#cli_tools", "Проект добавлен в группу @GolangGopher пользователем @author"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in %q", want, text)
		}
	}
}

func TestSessions(t *testing.T) {
	s := newSessions()
	s.set(1, session{Step: stepName, Name: "one"})
	got, ok := s.get(1)
	if !ok || got.Name != "one" {
		t.Fatalf("unexpected session: %+v %v", got, ok)
	}
	s.delete(1)
	if _, ok = s.get(1); ok {
		t.Fatal("session was not deleted")
	}
}

func TestProjectID(t *testing.T) {
	if id, ok := projectID("42"); !ok || id != 42 {
		t.Fatalf("unexpected id: %d %v", id, ok)
	}
	for _, value := range []string{"0", "-1", "abc"} {
		if _, ok := projectID(value); ok {
			t.Fatalf("accepted invalid id %q", value)
		}
	}
}

func TestRepoRefreshKeepsGoLanguage(t *testing.T) {
	p := repoProject(domain.Project{Language: "Go"}, gh.Repo{URL: "https://github.com/a/b", Language: "Rust", Topics: []string{"tool"}})
	if p.Language != "Go" {
		t.Fatalf("unexpected language: %s", p.Language)
	}
}

func TestCatalogParams(t *testing.T) {
	stars, contributors, offset, ok := catalogParams("50,1,6")
	if !ok || stars != 50 || !contributors || offset != 6 {
		t.Fatalf("unexpected params: %d %v %d %v", stars, contributors, offset, ok)
	}
	for _, value := range []string{"50,2,0", "-1,0,0", "x,0,0"} {
		if _, _, _, ok := catalogParams(value); ok {
			t.Fatalf("accepted invalid params %q", value)
		}
	}
}

func TestGroupMessageURL(t *testing.T) {
	b := Bot{cfg: config.Config{ProjectTargets: []config.ProjectTarget{{Language: "Go", ChatUsername: "@GolangGopher", ThreadID: 5}}}}
	got := b.groupMessageURL(domain.Project{Language: "Go", PublishedMessageID: 42})
	if got != "https://t.me/GolangGopher/42" {
		t.Fatalf("unexpected URL: %s", got)
	}
}

func TestChannelProjectLinksToGroup(t *testing.T) {
	text := formatChannelProject(domain.Project{Name: "Tool", Language: "Go", RepoURL: "https://github.com/a/b"}, "https://t.me/GolangGopher/42")
	if !strings.Contains(text, "Открыть публикацию в группе") || !strings.Contains(text, "https://t.me/GolangGopher/42") {
		t.Fatalf("missing group link: %s", text)
	}
}

func TestFormatProfileTags(t *testing.T) {
	got := formatProfileTags("NETWORK OWNER, Go-admin, trusted")
	if got != "#NETWORK_OWNER #Go_admin #trusted" {
		t.Fatalf("unexpected tags: %s", got)
	}
	if got := formatProfileTags(" "); got != "нет" {
		t.Fatalf("unexpected empty tags: %s", got)
	}
}

func TestButtonOwnership(t *testing.T) {
	b := Bot{buttonOwners: make(map[string]int64)}
	b.bindButtons(-1001, 42, 7)
	if !b.canUseButtons(-1001, 42, 7) {
		t.Fatal("owner cannot use buttons")
	}
	if b.canUseButtons(-1001, 42, 8) {
		t.Fatal("another user can use buttons")
	}
	if !b.canUseButtons(-1001, 43, 8) {
		t.Fatal("unbound public buttons were blocked")
	}
}
