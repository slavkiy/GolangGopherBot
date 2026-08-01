package bot

import (
	"strings"
	"testing"

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
