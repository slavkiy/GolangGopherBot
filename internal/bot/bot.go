package bot

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log"
	"strconv"
	"strings"
	"unicode"

	"golanggopherbot/internal/config"
	"golanggopherbot/internal/domain"
	gh "golanggopherbot/internal/github"
	"golanggopherbot/internal/store"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type Bot struct {
	api      *tgbotapi.BotAPI
	cfg      config.Config
	store    *store.Store
	github   *gh.Client
	sessions *sessions
}

var languages = []string{"Go", "Python", "JavaScript", "TypeScript", "Rust", "Java", "C#", "C++", "Другое"}

func New(cfg config.Config, s *store.Store) (*Bot, error) {
	api, err := tgbotapi.NewBotAPI(cfg.BotToken)
	if err != nil {
		return nil, err
	}
	return &Bot{api: api, cfg: cfg, store: s, github: gh.New(cfg.GitHubToken), sessions: newSessions()}, nil
}

func (b *Bot) Run(ctx context.Context) error {
	log.Printf("bot authorized as @%s", b.api.Self.UserName)
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 30
	updates := b.api.GetUpdatesChan(u)
	for {
		select {
		case <-ctx.Done():
			b.api.StopReceivingUpdates()
			return ctx.Err()
		case update, ok := <-updates:
			if !ok {
				return nil
			}
			b.handle(ctx, update)
		}
	}
}

func (b *Bot) handle(ctx context.Context, update tgbotapi.Update) {
	var from *tgbotapi.User
	if update.Message != nil {
		from = update.Message.From
	} else if update.CallbackQuery != nil {
		from = update.CallbackQuery.From
	} else {
		return
	}
	u, err := b.store.UpsertUser(ctx, domain.User{TelegramID: from.ID, Username: from.UserName, FirstName: from.FirstName, LastName: from.LastName, LanguageCode: from.LanguageCode})
	if err != nil {
		b.logErr(err)
		return
	}
	if u.IsBlocked {
		return
	}
	if update.CallbackQuery != nil {
		b.callback(ctx, u, update.CallbackQuery)
		return
	}
	m := update.Message
	if m.Chat.Type != "private" {
		b.groupCommand(ctx, m)
		return
	}
	if m.IsCommand() {
		b.command(ctx, u, m)
		return
	}
	if state, ok := b.sessions.get(from.ID); ok {
		b.input(ctx, u, m, state)
	}
}

func (b *Bot) command(ctx context.Context, u domain.User, m *tgbotapi.Message) {
	switch m.Command() {
	case "start":
		b.send(m.Chat.ID, "Привет! Здесь можно добавить open-source проект сообщества и найти проекты для участия.\n\n/project  добавить проект\n/myprojects  мои проекты\n/projects  смотреть проекты\n/cancel  отменить заполнение\n/help  помощь", nil)
	case "help":
		b.send(m.Chat.ID, "Команды:\n/project  новый проект\n/myprojects  управление моими проектами\n/projects  каталог и фильтры\n/cancel  отмена\n/admin  управление (для администраторов)", nil)
	case "project":
		b.sessions.set(u.TelegramID, session{Step: stepName})
		b.send(m.Chat.ID, "Как называется проект? Отправь название (до 80 символов).", nil)
	case "projects":
		b.projectFilters(m.Chat.ID)
	case "myprojects":
		b.showMyProjects(ctx, u, m.Chat.ID, nil)
	case "cancel":
		b.sessions.delete(u.TelegramID)
		b.send(m.Chat.ID, "Заполнение отменено.", nil)
	case "admin":
		if b.isAdmin(u.TelegramID) {
			b.adminHelp(m.Chat.ID)
		} else {
			b.send(m.Chat.ID, "Эта команда доступна только администраторам.", nil)
		}
	default:
		b.send(m.Chat.ID, "Неизвестная команда. Открой /help.", nil)
	}
}

func (b *Bot) input(ctx context.Context, u domain.User, m *tgbotapi.Message, s session) {
	text := strings.TrimSpace(m.Text)
	switch s.Step {
	case stepName:
		if len([]rune(text)) < 2 || len([]rune(text)) > 80 {
			b.send(m.Chat.ID, "Название должно быть от 2 до 80 символов.", nil)
			return
		}
		s.Name = text
		s.Step = stepDescription
		b.sessions.set(u.TelegramID, s)
		b.send(m.Chat.ID, "Добавь описание проекта: что он делает и чем может быть полезен (до 600 символов).", nil)
	case stepDescription:
		if len([]rune(text)) < 10 || len([]rune(text)) > 600 {
			b.send(m.Chat.ID, "Описание должно быть от 10 до 600 символов.", nil)
			return
		}
		s.AuthorDescription = text
		s.Step = stepLanguage
		b.sessions.set(u.TelegramID, s)
		b.languageMenu(m.Chat.ID, "Выбери основной язык:", "lang:")
	case stepRepo:
		b.send(m.Chat.ID, "Проверяю репозиторий…", nil)
		repo, err := b.github.Fetch(ctx, text)
		if err != nil {
			b.send(m.Chat.ID, "Не получилось получить репозиторий: "+err.Error()+"\nОтправь другую ссылку.", nil)
			return
		}
		s.RepoURL, s.Owner, s.RepoName, s.RepoDescription, s.Stars = repo.URL, repo.Owner, repo.Name, repo.Description, repo.Stars
		s.Topics = strings.Join(repo.Topics, ",")
		if s.Language == "Другое" && repo.Language != "" {
			s.Language = repo.Language
		}
		s.Step = stepContributors
		b.sessions.set(u.TelegramID, s)
		kb := tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Да, ищу участников", "contributors:yes"), tgbotapi.NewInlineKeyboardButtonData("Нет", "contributors:no")))
		b.send(m.Chat.ID, "Нужны ли проекту контрибьюторы?", &kb)
	case stepEditDescription:
		if len([]rune(text)) < 10 || len([]rune(text)) > 600 {
			b.send(m.Chat.ID, "Описание должно быть от 10 до 600 символов.", nil)
			return
		}
		if err := b.store.UpdateProjectDescription(ctx, s.ProjectID, u.ID, text); err != nil {
			b.operationError(m.Chat.ID, err)
			return
		}
		b.sessions.delete(u.TelegramID)
		b.syncProjectMessage(ctx, u, s.ProjectID)
		b.send(m.Chat.ID, "Описание обновлено.", nil)
	case stepEditRepo:
		b.send(m.Chat.ID, "Проверяю новый репозиторий…", nil)
		repo, err := b.github.Fetch(ctx, text)
		if err != nil {
			b.send(m.Chat.ID, "Не получилось получить репозиторий: "+err.Error()+"\nОтправь другую ссылку.", nil)
			return
		}
		current, err := b.store.ProjectForUser(ctx, s.ProjectID, u.ID)
		if err != nil {
			b.operationError(m.Chat.ID, err)
			return
		}
		updated := repoProject(current, repo)
		if err := b.store.UpdateProjectRepo(ctx, s.ProjectID, u.ID, updated); err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				b.send(m.Chat.ID, "Этот репозиторий уже привязан к другому проекту.", nil)
			} else {
				b.operationError(m.Chat.ID, err)
			}
			return
		}
		b.sessions.delete(u.TelegramID)
		b.syncProjectMessage(ctx, u, s.ProjectID)
		b.send(m.Chat.ID, "Ссылка и данные GitHub обновлены.", nil)
	}
}

func (b *Bot) callback(ctx context.Context, u domain.User, q *tgbotapi.CallbackQuery) {
	_, _ = b.api.Request(tgbotapi.NewCallback(q.ID, ""))
	parts := strings.SplitN(q.Data, ":", 2)
	if len(parts) != 2 {
		return
	}
	action, value := parts[0], parts[1]
	s, ok := b.sessions.get(u.TelegramID)
	switch action {
	case "lang":
		if !ok || s.Step != stepLanguage {
			return
		}
		s.Language = value
		s.Step = stepRepo
		b.sessions.set(u.TelegramID, s)
		b.edit(q, "Теперь отправь публичную ссылку на GitHub-репозиторий.", nil)
	case "contributors":
		if !ok || s.Step != stepContributors {
			return
		}
		s.WantsContributors = value == "yes"
		s.Step = stepConfirm
		b.sessions.set(u.TelegramID, s)
		kb := tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Опубликовать", "publish:yes"), tgbotapi.NewInlineKeyboardButtonData("Отмена", "publish:no")))
		b.edit(q, "Проверь карточку:\n\n"+formatDraft(s), &kb)
	case "publish":
		if !ok || s.Step != stepConfirm {
			return
		}
		if value == "no" {
			b.sessions.delete(u.TelegramID)
			b.edit(q, "Публикация отменена.", nil)
			return
		}
		b.publish(ctx, u, q, s)
	case "filter":
		b.showProjects(ctx, q.Message.Chat.ID, value, 0, q)
	case "page":
		vals := strings.Split(value, ",")
		if len(vals) == 2 {
			offset, _ := strconv.Atoi(vals[1])
			b.showProjects(ctx, q.Message.Chat.ID, vals[0], offset, q)
		}
	case "manage":
		b.showManageProject(ctx, u, q, value)
	case "update":
		b.showUpdateMenu(ctx, u, q, value)
	case "editdesc":
		b.startProjectEdit(ctx, u, q, value, stepEditDescription, "Отправь новое описание проекта (от 10 до 600 символов).")
	case "editrepo":
		b.startProjectEdit(ctx, u, q, value, stepEditRepo, "Отправь новую публичную ссылку на GitHub-репозиторий.")
	case "refresh":
		b.refreshProject(ctx, u, q, value)
	case "close":
		b.confirmClose(ctx, u, q, value)
	case "closeyes":
		b.closeProject(ctx, u, q, value)
	case "myprojects":
		b.showMyProjects(ctx, u, q.Message.Chat.ID, q)
	}
}

func (b *Bot) publish(ctx context.Context, u domain.User, q *tgbotapi.CallbackQuery, s session) {
	p, err := b.store.CreateProject(ctx, domain.Project{UserID: u.ID, Name: s.Name, Language: s.Language, RepoURL: s.RepoURL, RepoOwner: s.Owner, RepoName: s.RepoName, Description: s.RepoDescription, AuthorDescription: s.AuthorDescription, Topics: s.Topics, Stars: strconv.Itoa(s.Stars), WantsContributors: s.WantsContributors})
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			b.edit(q, "Этот репозиторий уже есть в каталоге.", nil)
		} else {
			b.logErr(err)
			b.edit(q, "Не удалось сохранить проект. Попробуй позже.", nil)
		}
		return
	}
	author := "@" + u.Username
	if u.Username == "" {
		author = strings.TrimSpace(u.FirstName + " " + u.LastName)
	}
	sent, err := b.publishMessage(formatPublishedProject(p, author, b.cfg.ProjectsChatUsername))
	if err != nil {
		_ = b.store.DeleteProject(ctx, p.ID)
		b.logErr(err)
		b.edit(q, "Публикация не удалась. Проверь настройки группы или попробуй позже.", nil)
		return
	}
	_ = b.store.SetPublication(ctx, p.ID, sent.Chat.ID, sent.MessageID)
	b.sessions.delete(u.TelegramID)
	b.edit(q, "Готово! Проект опубликован в ветке «Ваши проекты».", nil)
}

func (b *Bot) projectFilters(chatID int64) {
	rows := [][]tgbotapi.InlineKeyboardButton{{tgbotapi.NewInlineKeyboardButtonData("Все", "filter:all"), tgbotapi.NewInlineKeyboardButtonData("Ищут участников", "filter:contributors")}}
	for i := 0; i < len(languages); i += 3 {
		var row []tgbotapi.InlineKeyboardButton
		for j := i; j < i+3 && j < len(languages); j++ {
			row = append(row, tgbotapi.NewInlineKeyboardButtonData(languages[j], "filter:"+languages[j]))
		}
		rows = append(rows, row)
	}
	kb := tgbotapi.NewInlineKeyboardMarkup(rows...)
	b.send(chatID, "Выбери фильтр каталога:", &kb)
}

func (b *Bot) showProjects(ctx context.Context, chatID int64, filter string, offset int, q *tgbotapi.CallbackQuery) {
	lang := ""
	contributors := false
	if filter == "contributors" {
		contributors = true
	} else if filter != "all" {
		lang = filter
	}
	items, err := b.store.ListProjects(ctx, lang, contributors, 5, offset)
	if err != nil {
		b.logErr(err)
		return
	}
	text := "Проекты не найдены."
	if len(items) > 0 {
		var parts []string
		for _, p := range items {
			parts = append(parts, formatProject(p, ""))
		}
		text = strings.Join(parts, "\n\n──────────\n\n")
	}
	var row []tgbotapi.InlineKeyboardButton
	if offset > 0 {
		row = append(row, tgbotapi.NewInlineKeyboardButtonData("← Назад", fmt.Sprintf("page:%s,%d", filter, max(0, offset-5))))
	}
	if len(items) == 5 {
		row = append(row, tgbotapi.NewInlineKeyboardButtonData("Дальше →", fmt.Sprintf("page:%s,%d", filter, offset+5)))
	}
	var kb *tgbotapi.InlineKeyboardMarkup
	if len(row) > 0 {
		k := tgbotapi.NewInlineKeyboardMarkup(row)
		kb = &k
	}
	if q != nil {
		b.edit(q, text, kb)
	} else {
		b.send(chatID, text, kb)
	}
}

func (b *Bot) publishMessage(text string) (tgbotapi.Message, error) {
	chat := b.cfg.ProjectsChatUsername
	if b.cfg.ProjectsChatID != 0 {
		chat = strconv.FormatInt(b.cfg.ProjectsChatID, 10)
	}
	response, err := b.api.MakeRequest("sendMessage", tgbotapi.Params{
		"chat_id": chat, "message_thread_id": strconv.Itoa(b.cfg.ProjectsThreadID),
		"text": text, "parse_mode": "HTML",
	})
	if err != nil {
		return tgbotapi.Message{}, err
	}
	var message tgbotapi.Message
	if err := json.Unmarshal(response.Result, &message); err != nil {
		return tgbotapi.Message{}, err
	}
	return message, nil
}

func (b *Bot) editPublishedMessage(p domain.Project, text string) error {
	if p.PublishedChatID == 0 || p.PublishedMessageID == 0 {
		return fmt.Errorf("у проекта нет опубликованного сообщения")
	}
	_, err := b.api.MakeRequest("editMessageText", tgbotapi.Params{
		"chat_id": strconv.FormatInt(p.PublishedChatID, 10), "message_id": strconv.Itoa(p.PublishedMessageID),
		"text": text, "parse_mode": "HTML", "disable_web_page_preview": "false",
	})
	return err
}

func (b *Bot) showMyProjects(ctx context.Context, u domain.User, chatID int64, q *tgbotapi.CallbackQuery) {
	projects, err := b.store.ListUserProjects(ctx, u.ID)
	if err != nil {
		b.logErr(err)
		b.send(chatID, "Не удалось загрузить проекты.", nil)
		return
	}
	if len(projects) == 0 {
		if q != nil {
			b.edit(q, "У тебя пока нет проектов. Добавить: /project", nil)
		} else {
			b.send(chatID, "У тебя пока нет проектов. Добавить: /project", nil)
		}
		return
	}
	var text strings.Builder
	var rows [][]tgbotapi.InlineKeyboardButton
	text.WriteString("<b>Мои проекты</b>\n")
	for _, p := range projects {
		status := "опубликован"
		if p.Status == domain.StatusClosed {
			status = "закрыт"
		} else if p.Status != domain.StatusPublished {
			status = "скрыт"
		}
		fmt.Fprintf(&text, "\n%d. %s — %s", p.ID, esc(p.Name), status)
		if p.Status == domain.StatusPublished {
			rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("⚙️ "+shortButton(p.Name), fmt.Sprintf("manage:%d", p.ID))))
		}
	}
	kb := tgbotapi.NewInlineKeyboardMarkup(rows...)
	if q != nil {
		b.edit(q, text.String(), &kb)
	} else {
		b.send(chatID, text.String(), &kb)
	}
}

func (b *Bot) showManageProject(ctx context.Context, u domain.User, q *tgbotapi.CallbackQuery, value string) {
	id, ok := projectID(value)
	if !ok {
		return
	}
	p, err := b.store.ProjectForUser(ctx, id, u.ID)
	if err != nil || p.Status != domain.StatusPublished {
		b.edit(q, "Проект не найден или уже закрыт.", nil)
		return
	}
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("✏️ Обновить", fmt.Sprintf("update:%d", id)), tgbotapi.NewInlineKeyboardButtonData("🚫 Закрыть", fmt.Sprintf("close:%d", id))),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("← Мои проекты", "myprojects:list")),
	)
	b.edit(q, formatProject(p, ""), &kb)
}

func (b *Bot) showUpdateMenu(ctx context.Context, u domain.User, q *tgbotapi.CallbackQuery, value string) {
	id, ok := projectID(value)
	if !ok {
		return
	}
	p, err := b.store.ProjectForUser(ctx, id, u.ID)
	if err != nil || p.Status != domain.StatusPublished {
		b.edit(q, "Проект недоступен.", nil)
		return
	}
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Описание", fmt.Sprintf("editdesc:%d", id)), tgbotapi.NewInlineKeyboardButtonData("Ссылка", fmt.Sprintf("editrepo:%d", id))),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("↻ Взять данные из GitHub", fmt.Sprintf("refresh:%d", id))),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("← Назад", fmt.Sprintf("manage:%d", id))),
	)
	b.edit(q, "Что обновить в проекте <b>"+esc(p.Name)+"</b>?", &kb)
}

func (b *Bot) startProjectEdit(ctx context.Context, u domain.User, q *tgbotapi.CallbackQuery, value string, next step, prompt string) {
	id, ok := projectID(value)
	if !ok {
		return
	}
	p, err := b.store.ProjectForUser(ctx, id, u.ID)
	if err != nil || p.Status != domain.StatusPublished {
		b.edit(q, "Проект недоступен.", nil)
		return
	}
	b.sessions.set(u.TelegramID, session{Step: next, ProjectID: id})
	b.edit(q, prompt, nil)
}

func (b *Bot) refreshProject(ctx context.Context, u domain.User, q *tgbotapi.CallbackQuery, value string) {
	id, ok := projectID(value)
	if !ok {
		return
	}
	p, err := b.store.ProjectForUser(ctx, id, u.ID)
	if err != nil || p.Status != domain.StatusPublished {
		b.edit(q, "Проект недоступен.", nil)
		return
	}
	repo, err := b.github.Fetch(ctx, p.RepoURL)
	if err != nil {
		b.edit(q, "Не удалось обновить данные GitHub: "+esc(err.Error()), nil)
		return
	}
	if err = b.store.UpdateProjectRepo(ctx, id, u.ID, repoProject(p, repo)); err != nil {
		b.edit(q, "Не удалось сохранить обновление.", nil)
		b.logErr(err)
		return
	}
	b.syncProjectMessage(ctx, u, id)
	b.edit(q, "Данные GitHub и сообщение проекта обновлены.", nil)
}

func (b *Bot) confirmClose(ctx context.Context, u domain.User, q *tgbotapi.CallbackQuery, value string) {
	id, ok := projectID(value)
	if !ok {
		return
	}
	p, err := b.store.ProjectForUser(ctx, id, u.ID)
	if err != nil || p.Status != domain.StatusPublished {
		b.edit(q, "Проект недоступен.", nil)
		return
	}
	kb := tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Да, закрыть", fmt.Sprintf("closeyes:%d", id)), tgbotapi.NewInlineKeyboardButtonData("Отмена", fmt.Sprintf("manage:%d", id))))
	b.edit(q, "Закрыть проект <b>"+esc(p.Name)+"</b>? Он исчезнет из каталога, а публикация останется с отметкой о закрытии.", &kb)
}

func (b *Bot) closeProject(ctx context.Context, u domain.User, q *tgbotapi.CallbackQuery, value string) {
	id, ok := projectID(value)
	if !ok {
		return
	}
	p, err := b.store.ProjectForUser(ctx, id, u.ID)
	if err != nil || p.Status != domain.StatusPublished {
		b.edit(q, "Проект недоступен.", nil)
		return
	}
	if err = b.store.CloseProject(ctx, id, u.ID); err != nil {
		b.edit(q, "Не удалось закрыть проект.", nil)
		b.logErr(err)
		return
	}
	closed := fmt.Sprintf("<b>%s</b> закрыт по решению автора.", esc(p.Name))
	if err = b.editPublishedMessage(p, closed); err != nil {
		b.logErr(err)
		b.edit(q, "Проект закрыт в каталоге, но сообщение в группе обновить не удалось.", nil)
		return
	}
	b.edit(q, "Проект закрыт и скрыт из каталога.", nil)
}

func (b *Bot) syncProjectMessage(ctx context.Context, u domain.User, id int64) {
	p, err := b.store.ProjectForUser(ctx, id, u.ID)
	if err != nil {
		b.logErr(err)
		return
	}
	author := "@" + u.Username
	if u.Username == "" {
		author = strings.TrimSpace(u.FirstName + " " + u.LastName)
	}
	if err = b.editPublishedMessage(p, formatPublishedProject(p, author, b.cfg.ProjectsChatUsername)); err != nil {
		b.logErr(err)
	}
}

func repoProject(current domain.Project, repo gh.Repo) domain.Project {
	current.RepoURL, current.RepoOwner, current.RepoName, current.Description = repo.URL, repo.Owner, repo.Name, repo.Description
	current.Topics, current.Stars = strings.Join(repo.Topics, ","), strconv.Itoa(repo.Stars)
	if repo.Language != "" {
		current.Language = repo.Language
	}
	return current
}
func projectID(value string) (int64, bool) {
	id, err := strconv.ParseInt(value, 10, 64)
	return id, err == nil && id > 0
}
func shortButton(name string) string {
	r := []rune(name)
	if len(r) > 24 {
		return string(r[:24]) + "…"
	}
	return name
}
func (b *Bot) operationError(chatID int64, err error) {
	if errors.Is(err, sql.ErrNoRows) || strings.Contains(err.Error(), "unavailable") {
		b.send(chatID, "Проект не найден или уже закрыт.", nil)
		return
	}
	b.logErr(err)
	b.send(chatID, "Не удалось обновить проект. Попробуй позже.", nil)
}

func (b *Bot) groupCommand(ctx context.Context, m *tgbotapi.Message) {
	if !m.IsCommand() || !b.isAdmin(m.From.ID) {
		return
	}
	args := strings.Fields(m.CommandArguments())
	switch m.Command() {
	case "stats":
		x, e := b.store.Stats(ctx)
		if e == nil {
			b.send(m.Chat.ID, fmt.Sprintf("Пользователей: %d\nПроектов: %d\nИщут участников: %d", x.Users, x.Projects, x.Contributors), nil)
		}
	case "hideproject":
		b.adminProjectStatus(ctx, m, args, domain.StatusHidden)
	case "showproject":
		b.adminProjectStatus(ctx, m, args, domain.StatusPublished)
	case "block":
		b.adminBlock(ctx, m, args, true)
	case "unblock":
		b.adminBlock(ctx, m, args, false)
	}
}

func (b *Bot) adminProjectStatus(ctx context.Context, m *tgbotapi.Message, args []string, status string) {
	if len(args) != 1 {
		b.send(m.Chat.ID, "Укажи ID проекта.", nil)
		return
	}
	id, e := strconv.ParseInt(args[0], 10, 64)
	if e != nil {
		b.send(m.Chat.ID, "Некорректный ID.", nil)
		return
	}
	if e = b.store.SetProjectStatus(ctx, id, status); e != nil {
		b.send(m.Chat.ID, e.Error(), nil)
		return
	}
	b.send(m.Chat.ID, "Статус проекта обновлён.", nil)
}
func (b *Bot) adminBlock(ctx context.Context, m *tgbotapi.Message, args []string, blocked bool) {
	if len(args) != 1 {
		b.send(m.Chat.ID, "Укажи Telegram ID пользователя.", nil)
		return
	}
	id, e := strconv.ParseInt(args[0], 10, 64)
	if e != nil {
		b.send(m.Chat.ID, "Некорректный ID.", nil)
		return
	}
	if e = b.store.SetUserBlocked(ctx, id, blocked); e != nil {
		b.send(m.Chat.ID, e.Error(), nil)
		return
	}
	b.send(m.Chat.ID, "Статус пользователя обновлён.", nil)
}
func (b *Bot) adminHelp(chatID int64) {
	b.send(chatID, "Админ-команды (работают и в группе):\n/stats\n/hideproject ID\n/showproject ID\n/block TELEGRAM_ID\n/unblock TELEGRAM_ID", nil)
}
func (b *Bot) isAdmin(id int64) bool { _, ok := b.cfg.AdminIDs[id]; return ok }
func (b *Bot) send(chatID int64, text string, kb *tgbotapi.InlineKeyboardMarkup) {
	m := tgbotapi.NewMessage(chatID, text)
	m.ParseMode = "HTML"
	if kb != nil {
		m.ReplyMarkup = *kb
	}
	if _, err := b.api.Send(m); err != nil {
		b.logErr(err)
	}
}
func (b *Bot) edit(q *tgbotapi.CallbackQuery, text string, kb *tgbotapi.InlineKeyboardMarkup) {
	m := tgbotapi.NewEditMessageText(q.Message.Chat.ID, q.Message.MessageID, text)
	m.ParseMode = "HTML"
	if kb != nil {
		m.ReplyMarkup = kb
	}
	if _, err := b.api.Send(m); err != nil {
		b.logErr(err)
	}
}
func (b *Bot) languageMenu(chatID int64, text, prefix string) {
	var rows [][]tgbotapi.InlineKeyboardButton
	for i := 0; i < len(languages); i += 3 {
		var row []tgbotapi.InlineKeyboardButton
		for j := i; j < i+3 && j < len(languages); j++ {
			row = append(row, tgbotapi.NewInlineKeyboardButtonData(languages[j], prefix+languages[j]))
		}
		rows = append(rows, row)
	}
	kb := tgbotapi.NewInlineKeyboardMarkup(rows...)
	b.send(chatID, text, &kb)
}
func formatDraft(s session) string {
	p := domain.Project{Name: s.Name, Language: s.Language, RepoURL: s.RepoURL, Description: s.RepoDescription, AuthorDescription: s.AuthorDescription, Topics: s.Topics, Stars: strconv.Itoa(s.Stars), WantsContributors: s.WantsContributors}
	return formatProject(p, "")
}
func formatProject(p domain.Project, author string) string {
	desc := p.AuthorDescription
	if desc == "" {
		desc = p.Description
	}
	if desc == "" {
		desc = "Описание в репозитории не указано."
	}
	a := ""
	if author != "" && author != "@" {
		a = "\nАвтор: " + esc(author)
	}
	tags := topicHashtags(p.Topics)
	return fmt.Sprintf("<b>%s</b>\nЯзык: %s · ⭐ %s\nКонтрибьюторы: %s%s\n\n%s\n\n<a href=\"%s\">Открыть репозиторий</a>\n%s", esc(p.Name), esc(p.Language), esc(p.Stars), yesNo(p.WantsContributors), a, esc(desc), html.EscapeString(p.RepoURL), tags)
}

func formatPublishedProject(p domain.Project, author, groupUsername string) string {
	card := formatProject(p, "")
	group := groupUsername
	if group == "" {
		group = "@GolangGopher"
	} else if !strings.HasPrefix(group, "@") {
		group = "@" + group
	}
	if author == "" {
		author = "участником сообщества"
	}
	return fmt.Sprintf("%s\n\n<i>Проект добавлен в группу %s пользователем %s</i>", card, esc(group), esc(author))
}

func topicHashtags(topics string) string {
	result := []string{"#проект"}
	for _, topic := range strings.Split(topics, ",") {
		var clean []rune
		for _, r := range strings.TrimSpace(topic) {
			if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
				clean = append(clean, r)
			} else if r == '-' || unicode.IsSpace(r) {
				clean = append(clean, '_')
			}
		}
		if len(clean) > 0 {
			result = append(result, "#"+string(clean))
		}
		if len(result) == 11 {
			break
		}
	}
	return strings.Join(result, " ")
}
func yesNo(v bool) string {
	if v {
		return "нужны ✅"
	}
	return "не требуются"
}
func esc(v string) string { return html.EscapeString(v) }
func (b *Bot) logErr(err error) {
	if !errors.Is(err, sql.ErrNoRows) {
		log.Printf("bot error: %v", err)
	}
}
