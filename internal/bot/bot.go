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
		b.send(m.Chat.ID, "Привет! Здесь можно добавить open-source проект сообщества и найти проекты для участия.\n\n/project  добавить проект\n/projects  смотреть проекты\n/cancel  отменить заполнение\n/help  помощь", nil)
	case "help":
		b.send(m.Chat.ID, "Команды:\n/project  новый проект\n/projects  каталог и фильтры\n/cancel  отмена\n/admin  управление (для администраторов)", nil)
	case "project":
		b.sessions.set(u.TelegramID, session{Step: stepName})
		b.send(m.Chat.ID, "Как называется проект? Отправь название (до 80 символов).", nil)
	case "projects":
		b.projectFilters(m.Chat.ID)
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
	group := strings.TrimPrefix(groupUsername, "@")
	if group == "" {
		group = "GolangGopher"
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
