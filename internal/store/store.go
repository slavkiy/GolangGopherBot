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
	err := s.db.QueryRowContext(ctx, `SELECT id,telegram_id,username,first_name,last_name,language_code,is_blocked,created_at,updated_at FROM users WHERE telegram_id=?`, id).
		Scan(&u.ID, &u.TelegramID, &u.Username, &u.FirstName, &u.LastName, &u.LanguageCode, &u.IsBlocked, &u.CreatedAt, &u.UpdatedAt)
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
