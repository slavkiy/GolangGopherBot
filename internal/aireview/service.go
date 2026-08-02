package aireview

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	gh "golanggopherbot/internal/github"
)

type Service struct {
	baseURL string
	http    *http.Client
	github  *gh.Client
}

type Model struct {
	Name   string
	Label  string
	Vision bool
}

type ReviewResult struct {
	Model  string
	Text   string
	Vision bool
}

type Progress struct {
	Status string
	Text   string
	Done   bool
}

type snapshot struct {
	Tree          string
	Readme        string
	Manifests     string
	Files         string
	FileCount     int
	CodeFiles     int
	TestFiles     int
	ImageFiles    int
	ImagePayloads []string
}

type downloadInfo struct {
	SizeBytes int
}

func New(baseURL string, githubToken string) *Service {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "http://127.0.0.1:1234"
	}
	return &Service{
		baseURL: baseURL,
		http:    &http.Client{},
		github:  gh.New(githubToken),
	}
}

func (s *Service) AvailableModels(ctx context.Context) ([]Model, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("локальная модель недоступна: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("локальный сервер моделей вернул код %d", resp.StatusCode)
	}
	var data struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	models := make([]struct{ Name string `json:"name"` }, 0, len(data.Data))
	for _, item := range data.Data {
		models = append(models, struct{ Name string `json:"name"` }{Name: item.ID})
	}
	return s.preferredModels(ctx, models), nil
}

func (s *Service) Review(ctx context.Context, repoURL, model string) (ReviewResult, error) {
	var final ReviewResult
	_, err := s.ReviewStream(ctx, repoURL, model, func(p Progress) error {
		if p.Done {
			final = ReviewResult{Model: model, Text: p.Text, Vision: s.modelHasVision(ctx, model)}
		}
		return nil
	})
	return final, err
}

func (s *Service) ReviewStream(ctx context.Context, repoURL, model string, onProgress func(Progress) error) (ReviewResult, error) {
	if onProgress != nil {
		_ = onProgress(Progress{Status: "20% Получаю данные GitHub"})
	}
	repo, err := s.github.Fetch(ctx, repoURL)
	if err != nil {
		return ReviewResult{}, err
	}
	if onProgress != nil {
		_ = onProgress(Progress{Status: "40% Скачиваю архив репозитория"})
	}
	archive, info, err := s.downloadArchive(ctx, repo.URL, repo.Owner, repo.Name)
	if err != nil {
		return ReviewResult{}, err
	}
	if onProgress != nil {
		_ = onProgress(Progress{Status: "40% Архив скачан: " + humanBytes(info.SizeBytes)})
		_ = onProgress(Progress{Status: "60% Собираю срез проекта"})
	}
	snap, err := buildSnapshot(archive)
	if err != nil {
		return ReviewResult{}, err
	}
	if onProgress != nil {
		_ = onProgress(Progress{Status: fmt.Sprintf("60%% Найдено файлов: %d, кода: %d, тестов: %d, картинок: %d", snap.FileCount, snap.CodeFiles, snap.TestFiles, snap.ImageFiles)})
	}
	prompt := buildPrompt(repo, snap)
	useVision := s.modelHasVision(ctx, model)
	if onProgress != nil {
		status := "80% Запускаю обзор модели"
		if useVision && snap.ImageFiles > 0 {
			status = "80% Запускаю обзор модели с кодом и картинками"
		}
		_ = onProgress(Progress{Status: status})
	}
	text, err := s.generateStream(ctx, model, prompt, useVisionImages(useVision, snap.ImagePayloads), onProgress)
	if err != nil {
		return ReviewResult{}, err
	}
	result := ReviewResult{Model: model, Text: strings.TrimSpace(text), Vision: useVision}
	if onProgress != nil {
		_ = onProgress(Progress{Status: "95% Готовлю итог"})
		_ = onProgress(Progress{Done: true, Text: result.Text})
	}
	return result, nil
}

func (s *Service) preferredModels(ctx context.Context, models []struct{ Name string `json:"name"` }) []Model {
	preferred := []struct {
		key   string
		label string
	}{
		{key: "qwen", label: "Qwen"},
		{key: "gemma", label: "Gemma"},
		{key: "dolphin", label: "Dolphin"},
	}
	var result []Model
	used := make(map[string]bool)
	for _, family := range preferred {
		for _, model := range models {
			name := strings.TrimSpace(model.Name)
			if name == "" || used[name] {
				continue
			}
			if strings.Contains(strings.ToLower(name), family.key) {
				result = append(result, Model{Name: name, Label: family.label, Vision: s.modelHasVision(ctx, name)})
				used[name] = true
				break
			}
		}
	}
	if len(result) > 0 {
		return result
	}
	for i, model := range models {
		if i == 3 {
			break
		}
		name := strings.TrimSpace(model.Name)
		if name == "" {
			continue
		}
		result = append(result, Model{Name: name, Label: humanLabel(name), Vision: s.modelHasVision(ctx, name)})
	}
	return result
}

func humanLabel(name string) string {
	head := name
	if idx := strings.Index(head, "/"); idx > 0 {
		head = head[idx+1:]
	}
	if idx := strings.Index(head, ":"); idx > 0 {
		head = head[:idx]
	}
	head = strings.ReplaceAll(head, "-", " ")
	head = strings.ReplaceAll(head, "_", " ")
	if head == "" {
		return name
	}
	return strings.ToUpper(head[:1]) + head[1:]
}

func (s *Service) downloadArchive(ctx context.Context, repoURL, owner, name string) ([]byte, downloadInfo, error) {
	u, err := url.Parse(strings.TrimSpace(repoURL))
	if err != nil {
		return nil, downloadInfo{}, fmt.Errorf("неверная ссылка на репозиторий")
	}
	path := strings.Trim(u.Path, "/")
	if path != "" {
		parts := strings.Split(path, "/")
		if len(parts) >= 2 {
			owner, name = parts[0], strings.TrimSuffix(parts[1], ".git")
		}
	}
	tryURLs := []string{
		"https://codeload.github.com/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/zip/refs/heads/main",
		"https://codeload.github.com/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/zip/refs/heads/master",
	}
	var lastErr error
	for _, raw := range tryURLs {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
		if reqErr != nil {
			return nil, downloadInfo{}, reqErr
		}
		resp, doErr := s.http.Do(req)
		if doErr != nil {
			lastErr = doErr
			continue
		}
		if resp.StatusCode == http.StatusNotFound {
			resp.Body.Close()
			lastErr = fmt.Errorf("архив репозитория не найден")
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			lastErr = fmt.Errorf("GitHub вернул код %d при загрузке архива", resp.StatusCode)
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 24<<20))
		resp.Body.Close()
		if readErr != nil {
			return nil, downloadInfo{}, readErr
		}
		return data, downloadInfo{SizeBytes: len(data)}, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("не удалось загрузить архив")
	}
	return nil, downloadInfo{}, lastErr
}

func buildSnapshot(data []byte) (snapshot, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return snapshot{}, err
	}
	type fileEntry struct {
		name string
		size uint64
		body string
	}
	var (
		tree      []string
		selected  []fileEntry
		readme    string
		manifests []string
		codeFiles int
		testFiles int
		images    []string
	)
	for _, file := range reader.File {
		if file.FileInfo().IsDir() {
			continue
		}
		name := trimArchiveRoot(file.Name)
		if name == "" {
			continue
		}
		tree = append(tree, name)
		lower := strings.ToLower(filepath.Base(name))
		if isLikelyCodeFile(name) {
			codeFiles++
		}
		if isLikelyTestFile(name) {
			testFiles++
		}
		if isImageFile(name) && len(images) < 3 {
			if payload := readImagePayload(file); payload != "" {
				images = append(images, payload)
			}
		}
		if !shouldReadFile(name) {
			continue
		}
		rc, err := file.Open()
		if err != nil {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(rc, 5000))
		rc.Close()
		if err != nil {
			continue
		}
		text := sanitizeText(body)
		if text == "" {
			continue
		}
		switch {
		case readme == "" && strings.HasPrefix(lower, "readme"):
			readme = text
		case isManifestFile(name):
			manifests = append(manifests, "FILE: "+name+"\n"+text)
		}
		selected = append(selected, fileEntry{name: name, size: file.UncompressedSize64, body: text})
	}
	sort.Strings(tree)
	sort.SliceStable(selected, func(i, j int) bool {
		return fileRank(selected[i].name) < fileRank(selected[j].name)
	})
	if len(tree) > 140 {
		tree = tree[:140]
	}
	if len(selected) > 16 {
		selected = selected[:16]
	}
	if len(manifests) > 6 {
		manifests = manifests[:6]
	}
	var fileSnippets []string
	for _, item := range selected {
		fileSnippets = append(fileSnippets, "FILE: "+item.name+" ("+strconv.FormatUint(item.size, 10)+" bytes)\n"+item.body)
	}
	return snapshot{
		Tree:          strings.Join(tree, "\n"),
		Readme:        readme,
		Manifests:     strings.Join(manifests, "\n\n"),
		Files:         strings.Join(fileSnippets, "\n\n"),
		FileCount:     len(tree),
		CodeFiles:     codeFiles,
		TestFiles:     testFiles,
		ImageFiles:    len(images),
		ImagePayloads: images,
	}, nil
}

func trimArchiveRoot(name string) string {
	parts := strings.SplitN(name, "/", 2)
	if len(parts) != 2 {
		return ""
	}
	return parts[1]
}

func shouldReadFile(name string) bool {
	base := strings.ToLower(filepath.Base(name))
	switch base {
	case "readme.md", "readme", "license", "license.md", "makefile", "dockerfile", "go.mod", "package.json", "package-lock.json", "pnpm-lock.yaml", "yarn.lock", "cargo.toml", "cargo.lock", "pyproject.toml", "requirements.txt", "pom.xml", "build.gradle", "build.gradle.kts", "composer.json", "gemfile", "mix.exs":
		return true
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".go", ".js", ".ts", ".tsx", ".jsx", ".py", ".rs", ".java", ".kt", ".php", ".rb", ".swift", ".c", ".cc", ".cpp", ".h", ".hpp", ".cs", ".scala", ".sh", ".sql", ".md", ".txt", ".yaml", ".yml", ".json", ".toml", ".xml":
		return true
	default:
		return false
	}
}

func isManifestFile(name string) bool {
	switch strings.ToLower(filepath.Base(name)) {
	case "go.mod", "package.json", "cargo.toml", "pyproject.toml", "requirements.txt", "pom.xml", "build.gradle", "build.gradle.kts", "composer.json", "gemfile", "mix.exs", "dockerfile", "makefile":
		return true
	default:
		return false
	}
}

func isLikelyCodeFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".go", ".js", ".ts", ".tsx", ".jsx", ".py", ".rs", ".java", ".kt", ".php", ".rb", ".swift", ".c", ".cc", ".cpp", ".h", ".hpp", ".cs", ".scala":
		return true
	default:
		return false
	}
}

func isLikelyTestFile(name string) bool {
	lower := strings.ToLower(name)
	return strings.Contains(lower, "/test/") || strings.Contains(lower, "/tests/") || strings.HasSuffix(lower, "_test.go") || strings.HasSuffix(lower, ".spec.ts") || strings.HasSuffix(lower, ".spec.js") || strings.HasSuffix(lower, ".test.ts") || strings.HasSuffix(lower, ".test.js") || strings.HasPrefix(filepath.Base(lower), "test_")
}

func isImageFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".webp":
		return true
	default:
		return false
	}
}

func readImagePayload(file *zip.File) string {
	if file.UncompressedSize64 == 0 || file.UncompressedSize64 > 3<<20 {
		return ""
	}
	rc, err := file.Open()
	if err != nil {
		return ""
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, 3<<20))
	if err != nil || len(data) == 0 {
		return ""
	}
	return base64.StdEncoding.EncodeToString(data)
}

func sanitizeText(body []byte) string {
	text := strings.ReplaceAll(string(body), "\x00", "")
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	runes := []rune(text)
	if len(runes) > 2400 {
		text = string(runes[:2400]) + "\n...[truncated]"
	}
	return text
}

func fileRank(name string) int {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, "readme.md"), strings.HasSuffix(lower, "/readme.md"):
		return 0
	case isManifestFile(name):
		return 1
	case strings.HasSuffix(lower, ".md"):
		return 2
	case isLikelyCodeFile(name):
		return 3
	default:
		return 4
	}
}

func buildPrompt(repo gh.Repo, s snapshot) string {
	var prompt strings.Builder
	prompt.WriteString("Сделай честный технический обзор репозитория по статическому срезу.\n")
	prompt.WriteString("Важно: код не запускался, тесты не исполнялись, вывод только по коду, README, структуре проекта и открытым данным GitHub.\n")
	prompt.WriteString("Репозиторий может быть на любом языке и с любым стеком.\n")
	prompt.WriteString("Проверь: соответствие README и кода, архитектурные границы, обработку ошибок, edge cases, безопасность, качество API, поддержку тестами, признаки недоделанности.\n")
	if s.ImageFiles > 0 {
		prompt.WriteString("Если приложены изображения, учти их как часть документации и UI проекта.\n")
	}
	prompt.WriteString("Если данных не хватает, так и напиши, не выдумывай.\n")
	prompt.WriteString("Ответ строго на русском и коротко, максимум 2800 символов.\n")
	prompt.WriteString("Пиши именно в формате Telegram.\n")
	prompt.WriteString("Запрещено использовать markdown-заголовки вида ##, ###, списки с -, нумерацию 1. 2. 3. и длинные вводные фразы.\n")
	prompt.WriteString("Нужен такой формат:\n")
	prompt.WriteString("Кратко\nодин короткий абзац\n\n")
	prompt.WriteString("Сильные стороны\n• пункт\n• пункт\n\n")
	prompt.WriteString("Проблемы\n• пункт\n• пункт\n\n")
	prompt.WriteString("Риски\n• пункт\n• пункт\n\n")
	prompt.WriteString("Проверить вручную\n• пункт\n• пункт\n\n")
	prompt.WriteString("Оценка\nX/10\n\n")
	prompt.WriteString("Заголовки только обычным текстом, без решёток.\n")
	prompt.WriteString("Пункты только через символ • и короткие строки.\n\n")
	prompt.WriteString("REPOSITORY\n")
	prompt.WriteString("URL: " + repo.URL + "\n")
	prompt.WriteString("Owner: " + repo.Owner + "\n")
	prompt.WriteString("Name: " + repo.Name + "\n")
	prompt.WriteString("Primary language: " + repo.Language + "\n")
	prompt.WriteString("Stars: " + strconv.Itoa(repo.Stars) + "\n")
	prompt.WriteString("Topics: " + strings.Join(repo.Topics, ", ") + "\n")
	prompt.WriteString("Description: " + repo.Description + "\n\n")
	prompt.WriteString("Code files: " + strconv.Itoa(s.CodeFiles) + "\n")
	prompt.WriteString("Test files: " + strconv.Itoa(s.TestFiles) + "\n")
	prompt.WriteString("Image files sent to model: " + strconv.Itoa(s.ImageFiles) + "\n\n")
	if s.Readme != "" {
		prompt.WriteString("README\n" + s.Readme + "\n\n")
	}
	if s.Manifests != "" {
		prompt.WriteString("MANIFESTS\n" + s.Manifests + "\n\n")
	}
	prompt.WriteString("TREE\n" + s.Tree + "\n\n")
	if s.Files != "" {
		prompt.WriteString("FILE SNIPPETS\n" + s.Files + "\n")
	}
	return prompt.String()
}

func useVisionImages(enabled bool, images []string) []string {
	if !enabled || len(images) == 0 {
		return nil
	}
	return images
}

func (s *Service) generateStream(ctx context.Context, model, prompt string, images []string, onProgress func(Progress) error) (string, error) {
	content := []map[string]any{{"type": "text", "text": prompt}}
	for _, image := range images {
		content = append(content, map[string]any{
			"type": "image_url",
			"image_url": map[string]any{
				"url": "data:image/png;base64," + image,
			},
		})
	}
	payload := map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "user", "content": content},
		},
		"stream":      true,
		"temperature": 0.2,
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("локальная модель недоступна: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", fmt.Errorf("локальный сервер моделей вернул код %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var full strings.Builder
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(buf), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
		if payload == "[DONE]" {
			break
		}
		var item struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &item); err != nil {
			return "", err
		}
		if len(item.Choices) > 0 && item.Choices[0].Delta.Content != "" {
			full.WriteString(item.Choices[0].Delta.Content)
			if onProgress != nil {
				_ = onProgress(Progress{Text: full.String()})
			}
		}
	}
	out := strings.TrimSpace(full.String())
	if out == "" {
		return "", fmt.Errorf("локальная модель вернула пустой ответ")
	}
	return out, nil
}

func (s *Service) modelHasVision(ctx context.Context, model string) bool {
	return looksLikeVisionModel(model)
}

func looksLikeVisionModel(model string) bool {
	lower := strings.ToLower(model)
	return strings.Contains(lower, "vision") || strings.Contains(lower, "vl") || strings.Contains(lower, "gemma-3") || strings.Contains(lower, "gemma3") || strings.Contains(lower, "gemma4")
}

func humanBytes(size int) string {
	if size < 1024 {
		return strconv.Itoa(size) + " B"
	}
	if size < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(size)/1024)
	}
	return fmt.Sprintf("%.2f MB", float64(size)/(1024*1024))
}
