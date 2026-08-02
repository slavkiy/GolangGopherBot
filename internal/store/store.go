package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golanggopherbot/internal/domain"
	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS users (
 id INTEGER PRIMARY KEY AUTOINCREMENT, telegram_id INTEGER NOT NULL UNIQUE,
 username TEXT NOT NULL DEFAULT '', first_name TEXT NOT NULL DEFAULT '', last_name TEXT NOT NULL DEFAULT '',
 language_code TEXT NOT NULL DEFAULT '', is_blocked INTEGER NOT NULL DEFAULT 0,
 role TEXT NOT NULL DEFAULT 'member', tags TEXT NOT NULL DEFAULT '', warns INTEGER NOT NULL DEFAULT 0, activity_count INTEGER NOT NULL DEFAULT 0,
 created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP, updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS projects (
 id INTEGER PRIMARY KEY AUTOINCREMENT, user_id INTEGER NOT NULL REFERENCES users(id),
 name TEXT NOT NULL, language TEXT NOT NULL, repo_url TEXT NOT NULL UNIQUE,
 repo_owner TEXT NOT NULL DEFAULT '', repo_name TEXT NOT NULL DEFAULT '', description TEXT NOT NULL DEFAULT '',
 author_description TEXT NOT NULL DEFAULT '', topics TEXT NOT NULL DEFAULT '', stars TEXT NOT NULL DEFAULT '0',
 wants_contributors INTEGER NOT NULL DEFAULT 0, status TEXT NOT NULL DEFAULT 'published',
 published_chat_id INTEGER NOT NULL DEFAULT 0, published_message_id INTEGER NOT NULL DEFAULT 0,
 channel_chat_id INTEGER NOT NULL DEFAULT 0, channel_message_id INTEGER NOT NULL DEFAULT 0,
 created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP, updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_projects_language_status ON projects(language, status);
CREATE INDEX IF NOT EXISTS idx_projects_user ON projects(user_id);
CREATE TABLE IF NOT EXISTS network_groups(id INTEGER PRIMARY KEY AUTOINCREMENT,name TEXT NOT NULL,language TEXT NOT NULL UNIQUE,chat_id INTEGER NOT NULL UNIQUE,chat_username TEXT NOT NULL DEFAULT '',thread_id INTEGER NOT NULL,anti_spam INTEGER NOT NULL DEFAULT 0,spam_limit INTEGER NOT NULL DEFAULT 6,spam_window INTEGER NOT NULL DEFAULT 10,spam_action TEXT NOT NULL DEFAULT 'delete_warn',created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP);
CREATE TABLE IF NOT EXISTS custom_commands(name TEXT PRIMARY KEY,response TEXT NOT NULL,created_by INTEGER NOT NULL,created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP);
CREATE TABLE IF NOT EXISTS registered_groups(id INTEGER PRIMARY KEY AUTOINCREMENT,name TEXT NOT NULL,chat_id INTEGER NOT NULL UNIQUE,chat_username TEXT NOT NULL DEFAULT '',registered_by INTEGER NOT NULL,created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP);
`)
	if err != nil {
		return err
	}
	// Миграция уже созданных баз до появления описания автора и тем GitHub.
	for _, statement := range []string{
		`ALTER TABLE projects ADD COLUMN author_description TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE projects ADD COLUMN topics TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE projects ADD COLUMN channel_chat_id INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE projects ADD COLUMN channel_message_id INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE users ADD COLUMN role TEXT NOT NULL DEFAULT 'member'`,
		`ALTER TABLE users ADD COLUMN tags TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE users ADD COLUMN warns INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE users ADD COLUMN activity_count INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE network_groups ADD COLUMN spam_limit INTEGER NOT NULL DEFAULT 6`,
		`ALTER TABLE network_groups ADD COLUMN spam_window INTEGER NOT NULL DEFAULT 10`,
		`ALTER TABLE network_groups ADD COLUMN spam_action TEXT NOT NULL DEFAULT 'delete_warn'`,
	} {
		if _, alterErr := s.db.Exec(statement); alterErr != nil && !strings.Contains(alterErr.Error(), "duplicate column name") {
			return alterErr
		}
	}
	return nil
}

func (s *Store) UpsertUser(ctx context.Context, u domain.User) (domain.User, error) {
	_, err := s.db.ExecContext(ctx, `INSERT INTO users(telegram_id,username,first_name,last_name,language_code)
VALUES(?,?,?,?,?) ON CONFLICT(telegram_id) DO UPDATE SET username=excluded.username,first_name=excluded.first_name,last_name=excluded.last_name,language_code=excluded.language_code,updated_at=CURRENT_TIMESTAMP`, u.TelegramID, u.Username, u.FirstName, u.LastName, u.LanguageCode)
	if err != nil {
		return domain.User{}, err
	}
	return s.UserByTelegramID(ctx, u.TelegramID)
}

func (s *Store) UserByTelegramID(ctx context.Context, id int64) (domain.User, error) {
	var u domain.User
	err := s.db.QueryRowContext(ctx, `SELECT id,telegram_id,username,first_name,last_name,language_code,is_blocked,role,tags,warns,activity_count,created_at,updated_at FROM users WHERE telegram_id=?`, id).
		Scan(&u.ID, &u.TelegramID, &u.Username, &u.FirstName, &u.LastName, &u.LanguageCode, &u.IsBlocked, &u.Role, &u.Tags, &u.Warns, &u.ActivityCount, &u.CreatedAt, &u.UpdatedAt)
	return u, err
}

func (s *Store) UserByUsername(ctx context.Context, username string) (domain.User, error) {
	var u domain.User
	err := s.db.QueryRowContext(ctx, `SELECT id,telegram_id,username,first_name,last_name,language_code,is_blocked,role,tags,warns,activity_count,created_at,updated_at FROM users WHERE LOWER(username)=LOWER(?)`, strings.TrimPrefix(strings.TrimSpace(username), "@")).Scan(&u.ID, &u.TelegramID, &u.Username, &u.FirstName, &u.LastName, &u.LanguageCode, &u.IsBlocked, &u.Role, &u.Tags, &u.Warns, &u.ActivityCount, &u.CreatedAt, &u.UpdatedAt)
	return u, err
}

func (s *Store) CreateProject(ctx context.Context, p domain.Project) (domain.Project, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO projects(user_id,name,language,repo_url,repo_owner,repo_name,description,author_description,topics,stars,wants_contributors,status) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, p.UserID, p.Name, p.Language, p.RepoURL, p.RepoOwner, p.RepoName, p.Description, p.AuthorDescription, p.Topics, p.Stars, p.WantsContributors, domain.StatusPublished)
	if err != nil {
		return domain.Project{}, err
	}
	p.ID, err = res.LastInsertId()
	p.Status = domain.StatusPublished
	return p, err
}

func (s *Store) SetPublication(ctx context.Context, id, chatID int64, messageID int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE projects SET published_chat_id=?,published_message_id=?,updated_at=CURRENT_TIMESTAMP WHERE id=?`, chatID, messageID, id)
	return err
}

func (s *Store) SetChannelPublication(ctx context.Context, id, chatID int64, messageID int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE projects SET channel_chat_id=?,channel_message_id=?,updated_at=CURRENT_TIMESTAMP WHERE id=?`, chatID, messageID, id)
	return err
}

func (s *Store) DeleteProject(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM projects WHERE id=?`, id)
	return err
}

func (s *Store) ListProjects(ctx context.Context, minStars int, contributorsOnly bool, limit, offset int) ([]domain.Project, error) {
	query := `SELECT id,user_id,name,language,repo_url,repo_owner,repo_name,description,author_description,topics,stars,wants_contributors,status,published_chat_id,published_message_id,channel_chat_id,channel_message_id,created_at,updated_at FROM projects WHERE status=?`
	args := []any{domain.StatusPublished}
	if minStars > 0 {
		query += ` AND CAST(stars AS INTEGER)>=?`
		args = append(args, minStars)
	}
	if contributorsOnly {
		query += ` AND wants_contributors=1`
	}
	query += ` ORDER BY created_at DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Project
	for rows.Next() {
		var p domain.Project
		if err := rows.Scan(&p.ID, &p.UserID, &p.Name, &p.Language, &p.RepoURL, &p.RepoOwner, &p.RepoName, &p.Description, &p.AuthorDescription, &p.Topics, &p.Stars, &p.WantsContributors, &p.Status, &p.PublishedChatID, &p.PublishedMessageID, &p.ChannelChatID, &p.ChannelMessageID, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

const projectColumns = `id,user_id,name,language,repo_url,repo_owner,repo_name,description,author_description,topics,stars,wants_contributors,status,published_chat_id,published_message_id,channel_chat_id,channel_message_id,created_at,updated_at`

func scanProject(scanner interface{ Scan(...any) error }) (domain.Project, error) {
	var p domain.Project
	err := scanner.Scan(&p.ID, &p.UserID, &p.Name, &p.Language, &p.RepoURL, &p.RepoOwner, &p.RepoName, &p.Description, &p.AuthorDescription, &p.Topics, &p.Stars, &p.WantsContributors, &p.Status, &p.PublishedChatID, &p.PublishedMessageID, &p.ChannelChatID, &p.ChannelMessageID, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

func (s *Store) ProjectForUser(ctx context.Context, projectID, userID int64) (domain.Project, error) {
	return scanProject(s.db.QueryRowContext(ctx, `SELECT `+projectColumns+` FROM projects WHERE id=? AND user_id=?`, projectID, userID))
}

func (s *Store) ListUserProjects(ctx context.Context, userID int64) ([]domain.Project, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+projectColumns+` FROM projects WHERE user_id=? ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var projects []domain.Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}

func (s *Store) UpdateProjectDescription(ctx context.Context, projectID, userID int64, description string) error {
	return requireChanged(s.db.ExecContext(ctx, `UPDATE projects SET author_description=?,updated_at=CURRENT_TIMESTAMP WHERE id=? AND user_id=? AND status=?`, description, projectID, userID, domain.StatusPublished))
}

func (s *Store) UpdateProjectRepo(ctx context.Context, projectID, userID int64, p domain.Project) error {
	return requireChanged(s.db.ExecContext(ctx, `UPDATE projects SET repo_url=?,repo_owner=?,repo_name=?,description=?,topics=?,stars=?,language=?,updated_at=CURRENT_TIMESTAMP WHERE id=? AND user_id=? AND status=?`, p.RepoURL, p.RepoOwner, p.RepoName, p.Description, p.Topics, p.Stars, p.Language, projectID, userID, domain.StatusPublished))
}

func (s *Store) CloseProject(ctx context.Context, projectID, userID int64) error {
	return requireChanged(s.db.ExecContext(ctx, `UPDATE projects SET status=?,updated_at=CURRENT_TIMESTAMP WHERE id=? AND user_id=? AND status=?`, domain.StatusClosed, projectID, userID, domain.StatusPublished))
}

func requireChanged(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("project not found or unavailable")
	}
	return nil
}

func (s *Store) SetProjectStatus(ctx context.Context, id int64, status string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE projects SET status=?,updated_at=CURRENT_TIMESTAMP WHERE id=?`, status, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("project not found")
	}
	return nil
}
func (s *Store) SetUserBlocked(ctx context.Context, telegramID int64, blocked bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE users SET is_blocked=?,updated_at=CURRENT_TIMESTAMP WHERE telegram_id=?`, blocked, telegramID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("user not found")
	}
	return nil
}

type Stats struct{ Users, Projects, Contributors int }

func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var x Stats
	err := s.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM users),(SELECT COUNT(*) FROM projects WHERE status='published'),(SELECT COUNT(*) FROM projects WHERE status='published' AND wants_contributors=1)`).Scan(&x.Users, &x.Projects, &x.Contributors)
	return x, err
}

func (s *Store) UpsertNetworkGroup(ctx context.Context, g domain.NetworkGroup) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var antiSpam bool
	spamLimit, spamWindow, spamAction := 6, 10, "delete_warn"
	_ = tx.QueryRowContext(ctx, `SELECT anti_spam,spam_limit,spam_window,spam_action FROM network_groups WHERE chat_id=? OR language=? LIMIT 1`, g.ChatID, g.Language).Scan(&antiSpam, &spamLimit, &spamWindow, &spamAction)
	if _, err = tx.ExecContext(ctx, `DELETE FROM network_groups WHERE chat_id=? OR language=?`, g.ChatID, g.Language); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO network_groups(name,language,chat_id,chat_username,thread_id,anti_spam,spam_limit,spam_window,spam_action) VALUES(?,?,?,?,?,?,?,?,?)`, g.Name, g.Language, g.ChatID, g.ChatUsername, g.ThreadID, antiSpam, spamLimit, spamWindow, spamAction); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) NetworkGroups(ctx context.Context) ([]domain.NetworkGroup, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,language,chat_id,chat_username,thread_id,anti_spam,spam_limit,spam_window,spam_action FROM network_groups ORDER BY language`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.NetworkGroup
	for rows.Next() {
		var g domain.NetworkGroup
		if err := rows.Scan(&g.ID, &g.Name, &g.Language, &g.ChatID, &g.ChatUsername, &g.ThreadID, &g.AntiSpam, &g.SpamLimit, &g.SpamWindow, &g.SpamAction); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
func (s *Store) NetworkGroupForLanguage(ctx context.Context, language string) (domain.NetworkGroup, error) {
	var g domain.NetworkGroup
	err := s.db.QueryRowContext(ctx, `SELECT id,name,language,chat_id,chat_username,thread_id,anti_spam,spam_limit,spam_window,spam_action FROM network_groups WHERE language=?`, language).Scan(&g.ID, &g.Name, &g.Language, &g.ChatID, &g.ChatUsername, &g.ThreadID, &g.AntiSpam, &g.SpamLimit, &g.SpamWindow, &g.SpamAction)
	return g, err
}
func (s *Store) NetworkGroupByChat(ctx context.Context, chatID int64) (domain.NetworkGroup, error) {
	var g domain.NetworkGroup
	err := s.db.QueryRowContext(ctx, `SELECT id,name,language,chat_id,chat_username,thread_id,anti_spam,spam_limit,spam_window,spam_action FROM network_groups WHERE chat_id=?`, chatID).Scan(&g.ID, &g.Name, &g.Language, &g.ChatID, &g.ChatUsername, &g.ThreadID, &g.AntiSpam, &g.SpamLimit, &g.SpamWindow, &g.SpamAction)
	return g, err
}
func (s *Store) RemoveNetworkGroup(ctx context.Context, id int64) error {
	return requireChanged(s.db.ExecContext(ctx, `DELETE FROM network_groups WHERE id=?`, id))
}
func (s *Store) ToggleAntiSpam(ctx context.Context, chatID int64) (bool, error) {
	if err := requireChanged(s.db.ExecContext(ctx, `UPDATE network_groups SET anti_spam=CASE anti_spam WHEN 0 THEN 1 ELSE 0 END WHERE chat_id=?`, chatID)); err != nil {
		return false, err
	}
	g, err := s.NetworkGroupByChat(ctx, chatID)
	return g.AntiSpam, err
}
func (s *Store) SetAntiSpamLimit(ctx context.Context, chatID int64, limit int) error {
	return requireChanged(s.db.ExecContext(ctx, `UPDATE network_groups SET spam_limit=? WHERE chat_id=?`, limit, chatID))
}
func (s *Store) SetAntiSpamWindow(ctx context.Context, chatID int64, seconds int) error {
	return requireChanged(s.db.ExecContext(ctx, `UPDATE network_groups SET spam_window=? WHERE chat_id=?`, seconds, chatID))
}
func (s *Store) SetAntiSpamAction(ctx context.Context, chatID int64, action string) error {
	return requireChanged(s.db.ExecContext(ctx, `UPDATE network_groups SET spam_action=? WHERE chat_id=?`, action, chatID))
}
func (s *Store) IncrementActivity(ctx context.Context, telegramID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET activity_count=activity_count+1 WHERE telegram_id=?`, telegramID)
	return err
}
func (s *Store) SetRole(ctx context.Context, telegramID int64, role string) error {
	return requireChanged(s.db.ExecContext(ctx, `UPDATE users SET role=? WHERE telegram_id=?`, role, telegramID))
}
func (s *Store) AddWarn(ctx context.Context, telegramID int64) error {
	return requireChanged(s.db.ExecContext(ctx, `UPDATE users SET warns=warns+1 WHERE telegram_id=?`, telegramID))
}
func (s *Store) SetTags(ctx context.Context, telegramID int64, tags string) error {
	return requireChanged(s.db.ExecContext(ctx, `UPDATE users SET tags=? WHERE telegram_id=?`, tags, telegramID))
}
func (s *Store) UserProjectCount(ctx context.Context, userID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE user_id=?`, userID).Scan(&n)
	return n, err
}
func (s *Store) SaveCustomCommand(ctx context.Context, name, response string, createdBy int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO custom_commands(name,response,created_by) VALUES(?,?,?) ON CONFLICT(name) DO UPDATE SET response=excluded.response,created_by=excluded.created_by`, name, response, createdBy)
	return err
}
func (s *Store) CustomCommand(ctx context.Context, name string) (domain.CustomCommand, error) {
	var c domain.CustomCommand
	err := s.db.QueryRowContext(ctx, `SELECT name,response FROM custom_commands WHERE name=?`, name).Scan(&c.Name, &c.Response)
	return c, err
}
func (s *Store) RegisterGroup(ctx context.Context, g domain.RegisteredGroup, by int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO registered_groups(name,chat_id,chat_username,registered_by) VALUES(?,?,?,?) ON CONFLICT(chat_id) DO UPDATE SET name=excluded.name,chat_username=excluded.chat_username`, g.Name, g.ChatID, g.ChatUsername, by)
	return err
}
func (s *Store) RegisteredGroupByChat(ctx context.Context, chatID int64) (domain.RegisteredGroup, error) {
	var g domain.RegisteredGroup
	err := s.db.QueryRowContext(ctx, `SELECT id,name,chat_id,chat_username FROM registered_groups WHERE chat_id=?`, chatID).Scan(&g.ID, &g.Name, &g.ChatID, &g.ChatUsername)
	return g, err
}
func (s *Store) RegisteredGroups(ctx context.Context) ([]domain.RegisteredGroup, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,chat_id,chat_username FROM registered_groups ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.RegisteredGroup
	for rows.Next() {
		var g domain.RegisteredGroup
		if err := rows.Scan(&g.ID, &g.Name, &g.ChatID, &g.ChatUsername); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
func (s *Store) SanctionGroup(ctx context.Context, chatID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM network_groups WHERE chat_id=?`, chatID); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM registered_groups WHERE chat_id=?`, chatID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("group not registered")
	}
	return tx.Commit()
}
func (s *Store) PrivilegedUsers(ctx context.Context) ([]domain.User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,telegram_id,username,first_name,last_name,language_code,is_blocked,role,tags,warns,activity_count,created_at,updated_at FROM users WHERE role IN ('owner','admin','moderator')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.User
	for rows.Next() {
		var u domain.User
		if err := rows.Scan(&u.ID, &u.TelegramID, &u.Username, &u.FirstName, &u.LastName, &u.LanguageCode, &u.IsBlocked, &u.Role, &u.Tags, &u.Warns, &u.ActivityCount, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
func (s *Store) BlockedUsers(ctx context.Context) ([]domain.User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,telegram_id,username,first_name,last_name,language_code,is_blocked,role,tags,warns,activity_count,created_at,updated_at FROM users WHERE is_blocked=1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.User
	for rows.Next() {
		var u domain.User
		if err := rows.Scan(&u.ID, &u.TelegramID, &u.Username, &u.FirstName, &u.LastName, &u.LanguageCode, &u.IsBlocked, &u.Role, &u.Tags, &u.Warns, &u.ActivityCount, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
