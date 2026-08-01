package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Repo struct {
	URL, Owner, Name, Description string
	Stars                         int
	Language                      string
	Topics                        []string
}
type Client struct {
	http  *http.Client
	token string
}

func New(token string) *Client {
	return &Client{http: &http.Client{Timeout: 10 * time.Second}, token: token}
}

func (c *Client) Fetch(ctx context.Context, raw string) (Repo, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return Repo{}, fmt.Errorf("неверная ссылка")
	}
	if !strings.EqualFold(u.Host, "github.com") && !strings.EqualFold(u.Host, "www.github.com") {
		return Repo{}, fmt.Errorf("пока поддерживаются ссылки GitHub")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return Repo{}, fmt.Errorf("ссылка должна вести на репозиторий")
	}
	owner, name := parts[0], strings.TrimSuffix(parts[1], ".git")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name), nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "GolangGopherBot")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Repo{}, fmt.Errorf("GitHub недоступен: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return Repo{}, fmt.Errorf("репозиторий не найден или закрыт")
	}
	if resp.StatusCode != http.StatusOK {
		return Repo{}, fmt.Errorf("GitHub вернул код %d", resp.StatusCode)
	}
	var data struct {
		HTMLURL         string   `json:"html_url"`
		Language        string   `json:"language"`
		Description     *string  `json:"description"`
		StargazersCount int      `json:"stargazers_count"`
		Topics          []string `json:"topics"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return Repo{}, err
	}
	description := ""
	if data.Description != nil {
		description = *data.Description
	}
	return Repo{URL: data.HTMLURL, Owner: owner, Name: name, Description: description, Stars: data.StargazersCount, Language: data.Language, Topics: data.Topics}, nil
}
