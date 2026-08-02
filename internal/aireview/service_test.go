package aireview

import (
	"context"
	"testing"
)

func TestHumanLabel(t *testing.T) {
	if got := humanLabel("qwen2.5-coder:14b"); got != "Qwen2.5 coder" {
		t.Fatalf("unexpected label: %q", got)
	}
}

func TestLooksLikeVisionModel(t *testing.T) {
	if !looksLikeVisionModel("gemma4:12b") {
		t.Fatal("gemma4 should look like vision model")
	}
	if looksLikeVisionModel("qwen2.5:3b") {
		t.Fatal("plain qwen model should not look like vision model")
	}
}

func TestPreferredModels(t *testing.T) {
	s := New("", "")
	models := s.preferredModels(context.Background(), []struct {
		Name string `json:"name"`
	}{
		{Name: "gemma4:12b"},
		{Name: "qwen2.5:3b"},
		{Name: "dolphin3:8b"},
	})
	if len(models) != 3 {
		t.Fatalf("unexpected model count: %d", len(models))
	}
	if models[0].Name != "qwen2.5:3b" || models[1].Name != "gemma4:12b" || models[2].Name != "dolphin3:8b" {
		t.Fatalf("unexpected model order: %+v", models)
	}
}

func TestHelpers(t *testing.T) {
	if !isLikelyCodeFile("src/app.ts") {
		t.Fatal("ts file should be code")
	}
	if !isLikelyTestFile("pkg/tool_test.go") {
		t.Fatal("go test file should be detected")
	}
	if !isManifestFile("package.json") {
		t.Fatal("package.json should be manifest")
	}
	if !isImageFile("docs/screen.png") {
		t.Fatal("png should be image")
	}
}
