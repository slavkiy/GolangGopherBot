package bot

import (
	"strings"
	"testing"

	"golanggopherbot/internal/domain"
)

func TestFormatProjectEscapesHTML(t *testing.T) {
	text := formatProject(domain.Project{Name: "<tool>", Language: "Go", Stars: "2", Description: "A & B", RepoURL: "https://github.com/a/b", WantsContributors: true}, "@author")
	for _, want := range []string{"&lt;tool&gt;", "A &amp; B", "нужны ✅", "@author"} {
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
