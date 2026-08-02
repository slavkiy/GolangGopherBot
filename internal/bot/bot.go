package bot

import (
	"context"
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"golanggopherbot/internal/aireview"
	"golanggopherbot/internal/config"
	"golanggopherbot/internal/domain"
	gh "golanggopherbot/internal/github"
	"golanggopherbot/internal/store"

	tgbotapi "github.com/OvyFlash/telegram-bot-api"
)

type Bot struct {
	api          *tgbotapi.BotAPI
	cfg          config.Config
	store        *store.Store
	github       *gh.Client
	reviewer     *aireview.Service
	sessions     *sessions
	spamMu       sync.Mutex
	spam         map[string][]time.Time
	deleteMu     sync.Mutex
	deleteTimers map[string]*time.Timer
	buttonMu     sync.RWMutex
	buttonOwners map[string]int64
}

func New(cfg config.Config, s *store.Store) (*Bot, error) {
	api, err := tgbotapi.NewBotAPI(cfg.BotToken)
	if err != nil {
		return nil, err
	}
	return &Bot{api: api, cfg: cfg, store: s, github: gh.New(cfg.GitHubToken), reviewer: aireview.New(cfg.OllamaURL, cfg.GitHubToken), sessions: newSessions(), spam: make(map[string][]time.Time), deleteTimers: make(map[string]*time.Timer), buttonOwners: make(map[string]int64)}, nil
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
	if update.MyChatMember != nil {
		b.handleOwnMembership(update.MyChatMember)
		return
	}
	var from *tgbotapi.User
	if update.Message != nil {
		from = update.Message.From
	} else if update.CallbackQuery != nil {
		from = update.CallbackQuery.From
	} else {
		return
	}
	if from == nil {
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
	_ = b.store.IncrementActivity(ctx, u.TelegramID)
	u.ActivityCount++
	if update.CallbackQuery != nil {
		b.callback(ctx, u, update.CallbackQuery)
		return
	}
	m := update.Message
	if m.Chat.Type != "private" {
		if b.handleAutomationEvents(ctx, m) {
			return
		}
		if b.checkSpam(ctx, u, m) {
			return
		}
		if state, exists := b.sessions.get(u.TelegramID); exists && !m.IsCommand() {
			b.input(ctx, u, m, state)
			return
		}
		b.groupCommand(ctx, u, m)
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

func (b *Bot) handleOwnMembership(change *tgbotapi.ChatMemberUpdated) {
	status := change.NewChatMember.Status
	if change.Chat.Type == "private" || status == "left" || status == "kicked" {
		return
	}
	chatID := change.Chat.ID
	chatType := change.Chat.Type
	go func() {
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		<-timer.C
		var (
			missing []string
			err     error
		)
		if chatType == "channel" {
			missing, err = b.missingSyncRights(chatID, chatType)
		} else {
			missing, err = b.missingBotRights(chatID)
		}
		if err != nil || len(missing) == 0 {
			return
		}
		leaveText := "Бот покидает группу."
		if chatType == "channel" {
			leaveText = "Бот покидает канал."
		}
		b.send(chatID, "Для работы сети боту нужны административные права. Не хватает: "+esc(strings.Join(missing, ", "))+". "+leaveText, nil)
		_, _ = b.api.MakeRequest("leaveChat", tgbotapi.Params{"chat_id": strconv.FormatInt(chatID, 10)})
	}()
}

func (b *Bot) command(ctx context.Context, u domain.User, m *tgbotapi.Message) {
	switch m.Command() {
	case "start":
		b.sendInThread(m.Chat.ID, m.MessageThreadID, "Привет! Здесь можно добавить open-source проект сообщества и найти проекты для участия.\n\n/project  добавить проект\n/myprojects  мои проекты\n/projects  смотреть проекты\n/cancel  отменить заполнение\n/help  помощь", nil)
	case "help":
		b.sendInThread(m.Chat.ID, m.MessageThreadID, "Команды:\n/project  новый проект\n/myprojects  управление моими проектами\n/projects  каталог и фильтры\n/ai  обзор GitHub-проекта\n/cancel  отмена\n/admin  управление (для администраторов)", nil)
	case "project":
		b.sessions.set(u.TelegramID, session{Step: stepName})
		b.sendInThread(m.Chat.ID, m.MessageThreadID, "Как называется проект? Отправь название (до 80 символов).", nil)
	case "projects":
		b.projectFilters(m.Chat.ID)
	case "ai":
		b.startAIReview(ctx, u, m)
	case "myprojects":
		b.showMyProjects(ctx, u, m.Chat.ID, nil)
	case "cancel":
		b.sessions.delete(u.TelegramID)
		b.sendInThread(m.Chat.ID, m.MessageThreadID, "Заполнение отменено.", nil)
	case "admin":
		if b.canModerate(u) {
			b.adminMenu(ctx, u, m.Chat.ID, m.MessageThreadID)
		} else {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Эта команда доступна только администраторам.", nil)
		}
	case "info":
		b.showRequestedUserInfo(ctx, u, m)
	default:
		if !b.runCustomCommand(ctx, m) {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Неизвестная команда. Открой /help.", nil)
		}
	}
}

func (b *Bot) input(ctx context.Context, u domain.User, m *tgbotapi.Message, s session) {
	text := strings.TrimSpace(m.Text)
	switch s.Step {
	case stepName:
		if len([]rune(text)) < 2 || len([]rune(text)) > 80 {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Название должно быть от 2 до 80 символов.", nil)
			return
		}
		s.Name = text
		s.Step = stepDescription
		b.sessions.set(u.TelegramID, s)
		b.sendInThread(m.Chat.ID, m.MessageThreadID, "Добавь описание проекта: что он делает и чем может быть полезен (до 600 символов).", nil)
	case stepDescription:
		if len([]rune(text)) < 10 || len([]rune(text)) > 600 {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Описание должно быть от 10 до 600 символов.", nil)
			return
		}
		s.AuthorDescription = text
		s.Step = stepLanguage
		b.sessions.set(u.TelegramID, s)
		b.languageMenu(m.Chat.ID)
	case stepRepo:
		b.sendInThread(m.Chat.ID, m.MessageThreadID, "Проверяю репозиторий…", nil)
		repo, err := b.github.Fetch(ctx, text)
		if err != nil {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Не получилось получить репозиторий: "+err.Error()+"\nОтправь другую ссылку.", nil)
			return
		}
		s.RepoURL, s.Owner, s.RepoName, s.RepoDescription, s.Stars = repo.URL, repo.Owner, repo.Name, repo.Description, repo.Stars
		s.Topics = strings.Join(repo.Topics, ",")
		s.Step = stepContributors
		b.sessions.set(u.TelegramID, s)
		kb := tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Да, ищу участников", "contributors:yes"), tgbotapi.NewInlineKeyboardButtonData("Нет", "contributors:no")))
		b.sendInThread(m.Chat.ID, m.MessageThreadID, "Нужны ли проекту контрибьюторы?", &kb)
	case stepEditDescription:
		if len([]rune(text)) < 10 || len([]rune(text)) > 600 {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Описание должно быть от 10 до 600 символов.", nil)
			return
		}
		if err := b.store.UpdateProjectDescription(ctx, s.ProjectID, u.ID, text); err != nil {
			b.operationError(m.Chat.ID, err)
			return
		}
		b.sessions.delete(u.TelegramID)
		b.syncProjectMessage(ctx, u, s.ProjectID)
		b.sendInThread(m.Chat.ID, m.MessageThreadID, "Описание обновлено.", nil)
	case stepEditRepo:
		b.sendInThread(m.Chat.ID, m.MessageThreadID, "Проверяю новый репозиторий…", nil)
		repo, err := b.github.Fetch(ctx, text)
		if err != nil {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Не получилось получить репозиторий: "+err.Error()+"\nОтправь другую ссылку.", nil)
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
				b.sendInThread(m.Chat.ID, m.MessageThreadID, "Этот репозиторий уже привязан к другому проекту.", nil)
			} else {
				b.operationError(m.Chat.ID, err)
			}
			return
		}
		b.sessions.delete(u.TelegramID)
		b.syncProjectMessage(ctx, u, s.ProjectID)
		b.sendInThread(m.Chat.ID, m.MessageThreadID, "Ссылка и данные GitHub обновлены.", nil)
	case stepAdminAddRoute:
		b.deleteAdminInput(m)
		language := strings.TrimSpace(text)
		if language == "" || len([]rune(language)) > 32 {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Отправь только название языка, например: Go", nil)
			return
		}
		registered, err := b.store.RegisteredGroupByChat(ctx, s.AdminChatID)
		if err != nil {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Сначала зарегистрируй группу кнопкой «Регистрация».", nil)
			return
		}
		err = b.store.UpsertNetworkGroup(ctx, domain.NetworkGroup{Name: registered.Name, Language: language, ChatID: s.AdminChatID, ChatUsername: s.AdminChatUsername, ThreadID: s.AdminThreadID})
		b.sessions.delete(u.TelegramID)
		if err != nil {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Не удалось сохранить маршрут.", nil)
		} else {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Группа и тема проектов сохранены.", nil)
		}
	case stepAdminRole:
		b.deleteAdminInput(m)
		parts := strings.Fields(text)
		role, reference := "", ""
		if m.ReplyToMessage != nil && len(parts) == 1 {
			role = parts[0]
		} else if len(parts) == 2 {
			reference, role = parts[0], parts[1]
		} else {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Ответь на сообщение текстом ROLE или отправь: @username ROLE", nil)
			return
		}
		valid := map[string]bool{"owner": true, "admin": true, "moderator": true, "member": true}
		if !valid[role] {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Роль: owner, admin, moderator или member.", nil)
			return
		}
		target, err := b.resolveUserReference(ctx, m, reference)
		if err != nil {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Пользователь не найден.", nil)
			return
		}
		err = b.store.SetRole(ctx, target.TelegramID, role)
		b.sessions.delete(u.TelegramID)
		if err != nil {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Пользователь сначала должен открыть бота.", nil)
		} else {
			target.Role = role
			failures := b.syncUserRole(ctx, target)
			if len(failures) > 0 {
				b.sendInThread(m.Chat.ID, m.MessageThreadID, "Роль сохранена, но Telegram отказал в группах: "+esc(strings.Join(failures, ", ")), nil)
			} else {
				b.sendInThread(m.Chat.ID, m.MessageThreadID, "Роль обновлена во всей сети.", nil)
			}
		}
	case stepAdminWarn:
		b.deleteAdminInput(m)
		target, err := b.resolveUserReference(ctx, m, text)
		if err != nil {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Ответь на сообщение или отправь @username / ID.", nil)
			return
		}
		err = b.addWarnAndRun(ctx, target, m)
		b.sessions.delete(u.TelegramID)
		if err != nil {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Пользователь не найден.", nil)
		} else {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Варн добавлен.", nil)
		}
	case stepAdminTag:
		b.deleteAdminInput(m)
		reference, tags := "", text
		if m.ReplyToMessage == nil {
			parts := strings.SplitN(text, " ", 2)
			if len(parts) != 2 {
				b.sendInThread(m.Chat.ID, m.MessageThreadID, "Ответь тегами на сообщение или отправь: @username TAGS", nil)
				return
			}
			reference, tags = parts[0], parts[1]
		}
		target, err := b.resolveUserReference(ctx, m, reference)
		if err != nil {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Пользователь не найден.", nil)
			return
		}
		err = b.store.SetTags(ctx, target.TelegramID, tags)
		b.sessions.delete(u.TelegramID)
		if err != nil {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Пользователь не найден.", nil)
		} else {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Теги обновлены.", nil)
		}
	case stepAdminCommand:
		b.deleteAdminInput(m)
		parts := strings.SplitN(text, " ", 2)
		if len(parts) != 2 {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Отправь: NAME ТЕКСТ", nil)
			return
		}
		name := strings.TrimPrefix(strings.ToLower(parts[0]), "/")
		err := b.store.SaveCustomCommand(ctx, name, parts[1], u.TelegramID)
		b.sessions.delete(u.TelegramID)
		if err != nil {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Не удалось сохранить команду.", nil)
		} else {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Команда /"+esc(name)+" сохранена.", nil)
		}
	case stepAdminBlock, stepAdminUnblock:
		b.deleteAdminInput(m)
		target, err := b.resolveUserReference(ctx, m, text)
		if err != nil {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Ответь на сообщение или отправь @username / ID.", nil)
			return
		}
		blocked := s.Step == stepAdminBlock
		failures, err := b.setNetworkBlock(ctx, target.TelegramID, blocked)
		b.sessions.delete(u.TelegramID)
		if err != nil {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Пользователь не найден.", nil)
		} else if len(failures) > 0 {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Статус сети обновлён, но не удалось применить действие в группах: "+esc(strings.Join(failures, ", ")), nil)
		} else if blocked {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Пользователь заблокирован во всей сети.", nil)
		} else {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Пользователь разблокирован во всей сети.", nil)
		}
	case stepAdminRegisterChannel:
		if m.Chat.Type != "private" {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Перешли запись из канала в личный диалог с ботом.", nil)
			return
		}
		channel := forwardedChannel(m)
		if channel == nil {
			b.sendInThread(m.Chat.ID, 0, "Нужна пересланная запись из канала. Если канал запретил пересылку, временно разреши её в настройках канала.", nil)
			return
		}
		if err := b.registerChannel(ctx, u, channel); err != nil {
			b.sendInThread(m.Chat.ID, 0, "Не удалось зарегистрировать канал: "+esc(err.Error()), nil)
			return
		}
		b.sessions.delete(u.TelegramID)
		failures := b.syncNetworkAdmins(ctx)
		if len(failures) > 0 {
			b.sendInThread(m.Chat.ID, 0, "Канал зарегистрирован. Синхронизация продолжена по всей сети, но есть ошибки:\n"+esc(strings.Join(failures, "\n")), nil)
		} else {
			b.sendInThread(m.Chat.ID, 0, "Канал зарегистрирован, роли синхронизированы во всей сети.", nil)
		}
	case stepAdminAutomationValue:
		value := strings.TrimSpace(m.Text)
		ruleValue, err := b.buildAutomationValue(s, value)
		if err != nil {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, err.Error(), nil)
			return
		}
		if err := b.store.AddAutomationRule(ctx, domain.AutomationRule{ChatID: s.AdminChatID, Event: s.AutomationEvent, Action: s.AutomationAction, Value: ruleValue}, u.TelegramID); err != nil {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Не удалось сохранить событие.", nil)
			return
		}
		b.sessions.delete(u.TelegramID)
		b.sendInThread(m.Chat.ID, m.MessageThreadID, "Событие сохранено.", nil)
	case stepAdminBroadcastText:
		text := strings.TrimSpace(m.Text)
		if text != "" && len(m.Entities) == 0 && looksLikeBroadcastHTML(text) {
			if len([]rune(text)) > 3500 {
				b.sendInThread(m.Chat.ID, m.MessageThreadID, "HTML-текст должен быть до 3500 символов.", nil)
				return
			}
			if err := validateBroadcastHTML(text); err != nil {
				b.sendInThread(m.Chat.ID, m.MessageThreadID, "Ошибка HTML: "+esc(err.Error())+"\nИсправь разметку и отправь сообщение ещё раз.", nil)
				return
			}
			s.BroadcastMode, s.BroadcastText = "html", text
		} else {
			s.BroadcastMode, s.BroadcastChatID, s.BroadcastMessageID = "copy", m.Chat.ID, m.MessageID
		}
		s.Step = stepAdminBroadcastConfirm
		b.sessions.set(u.TelegramID, s)
		kb := tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Отправить", "admbroadcastsend:run"), tgbotapi.NewInlineKeyboardButtonData("Отмена", "admmenu:show")))
		preview := "<b>Предпросмотр рассылки</b>\nПолучатели: " + broadcastScopeName(s.BroadcastScope)
		if s.BroadcastMode == "html" {
			preview += "\n\n" + text
		} else {
			preview += "\n\nБудет скопировано сообщение выше со всеми медиа и форматированием Telegram."
		}
		b.sendOwnedInThread(m.Chat.ID, m.MessageThreadID, preview, &kb, u.TelegramID)
	}
}

func (b *Bot) callback(ctx context.Context, u domain.User, q *tgbotapi.CallbackQuery) {
	if q.Message != nil && !b.canUseButtons(q.Message.Chat.ID, q.Message.MessageID, u.TelegramID) {
		_, _ = b.api.Request(tgbotapi.NewCallbackWithAlert(q.ID, "Это меню открыто другим пользователем."))
		return
	}
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
		if _, exists := b.targetForLanguage(ctx, value); !exists {
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
	case "stars":
		b.contributorsFilter(q, value)
	case "catalog":
		minStars, contributors, offset, valid := catalogParams(value)
		if valid {
			b.showProjects(ctx, q.Message.Chat.ID, minStars, contributors, offset, q)
		}
	case "filters":
		b.projectFiltersEdit(q)
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
	case "aimodel":
		if !ok || s.Step != stepAIModel || s.AIRepoURL == "" {
			return
		}
		b.sessions.delete(u.TelegramID)
		b.edit(q, "Проверяю репозиторий. Это может занять до пары минут.", nil)
		go b.runAIReview(context.Background(), q.Message.Chat.ID, q.Message.MessageThreadID, s.AIRepoURL, value)
	case "aicancel":
		if ok && s.Step == stepAIModel {
			b.sessions.delete(u.TelegramID)
		}
		b.edit(q, "Обзор отменён.", nil)
	case "admstats":
		if b.canModerate(u) {
			x, e := b.store.Stats(ctx)
			if e == nil {
				b.edit(q, fmt.Sprintf("Пользователей: %d\nПроектов: %d\nИщут участников: %d", x.Users, x.Projects, x.Contributors), b.adminBackKeyboard())
			}
		}
	case "admgroup":
		if b.isOwner(u) {
			b.adminGroups(ctx, q)
		}
	case "admdelgroup":
		if b.isOwner(u) {
			id, _ := strconv.ParseInt(value, 10, 64)
			if err := b.store.RemoveNetworkGroup(ctx, id); err != nil {
				b.edit(q, "Не удалось отключить группу.", b.adminBackKeyboard())
			} else {
				b.adminGroups(ctx, q)
			}
		}
	case "admantispam":
		if b.canModerate(u) {
			b.antiSpamMenu(ctx, q)
		}
	case "admcat":
		if b.canModerate(u) {
			b.adminCategory(u, q, value)
		}
	case "antitoggle":
		if b.canModerate(u) {
			_, _ = b.store.ToggleAntiSpam(ctx, q.Message.Chat.ID)
			b.antiSpamMenu(ctx, q)
		}
	case "antilimit":
		if b.canModerate(u) {
			v, _ := strconv.Atoi(value)
			if v == 3 || v == 6 || v == 10 || v == 15 {
				_ = b.store.SetAntiSpamLimit(ctx, q.Message.Chat.ID, v)
			}
			b.antiSpamMenu(ctx, q)
		}
	case "antiwindow":
		if b.canModerate(u) {
			v, _ := strconv.Atoi(value)
			if v == 5 || v == 10 || v == 30 || v == 60 {
				_ = b.store.SetAntiSpamWindow(ctx, q.Message.Chat.ID, v)
			}
			b.antiSpamMenu(ctx, q)
		}
	case "antiaction":
		if b.canModerate(u) && (value == "delete" || value == "delete_warn") {
			_ = b.store.SetAntiSpamAction(ctx, q.Message.Chat.ID, value)
			b.antiSpamMenu(ctx, q)
		}
	case "admmenu":
		if b.canModerate(u) {
			b.sessions.delete(u.TelegramID)
			b.adminMenuEdit(ctx, u, q)
		}
	case "admsanction":
		if b.isOwner(u) {
			chatID, _ := strconv.ParseInt(value, 10, 64)
			if err := b.store.SanctionGroup(ctx, chatID); err != nil {
				b.edit(q, "Группа не зарегистрирована.", b.adminBackKeyboard())
				return
			}
			b.edit(q, "Группа исключена из сети. Бот покидает группу.", nil)
			_, _ = b.api.MakeRequest("leaveChat", tgbotapi.Params{"chat_id": strconv.FormatInt(chatID, 10)})
		}
	case "admsync":
		if b.isOwner(u) {
			failures := b.syncNetworkAdmins(ctx)
			if len(failures) > 0 {
				b.edit(q, "Синхронизация выполнена по всей сети. Ошибки:\n"+esc(strings.Join(failures, "\n")), b.adminBackKeyboard())
			} else {
				b.edit(q, "Роли синхронизированы во всей сети.", b.adminBackKeyboard())
			}
		}
	case "admsanctionask":
		if b.isOwner(u) {
			kb := tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Подтвердить исключение", fmt.Sprintf("admsanction:%d", q.Message.Chat.ID)), tgbotapi.NewInlineKeyboardButtonData("Отмена", "admmenu:show")))
			b.edit(q, "Исключить группу из сети и вывести бота? История и участники сохранятся.", &kb)
		}
	case "admregister":
		if b.isOwner(u) {
			b.registerGroupFromButton(ctx, u, q)
		}
	case "admchannel":
		if b.isOwner(u) {
			if q.Message.Chat.Type != "private" {
				b.edit(q, "Регистрация канала запускается в личном диалоге с ботом.", b.adminBackKeyboard())
				return
			}
			b.sessions.set(u.TelegramID, session{Step: stepAdminRegisterChannel})
			b.edit(q, "Перешли сюда любую запись из канала. Бот возьмёт название и ID сам.", b.adminBackKeyboard())
		}
	case "admbroadcast":
		if b.isOwner(u) {
			b.broadcastScopeMenu(q)
		}
	case "admbroadcastscope":
		if b.isOwner(u) && validBroadcastScope(value) {
			b.sessions.set(u.TelegramID, session{Step: stepAdminBroadcastText, BroadcastScope: value})
			b.edit(q, "Отправь сообщение для рассылки. Поддерживаются текст, фото, видео, анимации, документы, аудио, голосовые, видеосообщения, стикеры, опросы и подписи. Форматирование Telegram и Premium emoji сохраняются.\n\nДля ручного HTML: &lt;b&gt;, &lt;i&gt;, &lt;u&gt;, &lt;s&gt;, &lt;code&gt;, &lt;pre&gt;, &lt;blockquote&gt;, &lt;tg-spoiler&gt;, &lt;tg-emoji emoji-id=\"...\"&gt;, &lt;a href=\"https://...\"&gt;.", b.adminBackKeyboard())
		}
	case "admbroadcastsend":
		if b.isOwner(u) {
			s, ok := b.sessions.get(u.TelegramID)
			if !ok || s.Step != stepAdminBroadcastConfirm {
				return
			}
			b.sessions.delete(u.TelegramID)
			sent, failures := b.sendBroadcast(ctx, s)
			result := fmt.Sprintf("Рассылка завершена. Отправлено: %d.", sent)
			if len(failures) > 0 {
				result += "\nОшибки:\n" + esc(strings.Join(failures, "\n"))
			}
			b.edit(q, result, b.adminBackKeyboard())
		}
	case "admsettings":
		if b.isOwner(u) {
			b.networkSettings(ctx, q)
		}
	case "admautodelete":
		if b.isOwner(u) {
			seconds, err := strconv.Atoi(value)
			if err == nil && (seconds == 0 || seconds == 15 || seconds == 30 || seconds == 60 || seconds == 120) {
				_ = b.store.SetNetworkSetting(ctx, "service_delete_seconds", value)
			}
			b.networkSettings(ctx, q)
		}
	case "admevents":
		if b.canModerate(u) {
			b.automationScopeMenu(u, q)
		}
	case "admeventscope":
		if b.canModerate(u) && (value == "local" || value == "global") {
			if value == "global" && !b.isOwner(u) {
				return
			}
			b.automationMenu(ctx, u, q, value)
		}
	case "admevent":
		if b.canModerate(u) {
			parts := strings.SplitN(value, ",", 2)
			if len(parts) == 2 && (parts[0] == "local" || parts[0] == "global") {
				if parts[0] == "global" && !b.isOwner(u) {
					return
				}
				b.automationActionMenu(q, parts[0], parts[1])
			}
		}
	case "admaction":
		if b.canModerate(u) {
			parts := strings.SplitN(value, ",", 3)
			if len(parts) == 3 {
				b.startAutomationAction(ctx, u, q, parts[0], parts[1], parts[2])
			}
		}
	case "admeventdel":
		if b.canModerate(u) {
			parts := strings.SplitN(value, ",", 2)
			if len(parts) != 2 {
				return
			}
			scope, idText := parts[0], parts[1]
			if scope == "global" && !b.isOwner(u) {
				return
			}
			chatID := q.Message.Chat.ID
			if scope == "global" {
				chatID = 0
			}
			id, _ := strconv.ParseInt(idText, 10, 64)
			_ = b.store.RemoveAutomationRule(ctx, id, chatID)
			b.automationMenu(ctx, u, q, scope)
		}
	case "admadd":
		if b.isOwner(u) {
			b.startAdminSession(u, q, stepAdminAddRoute, "Отправь только язык этой группы, например: Go", true)
		}
	case "admrole":
		if b.isOwner(u) {
			b.startAdminSession(u, q, stepAdminRole, "Ответь на сообщение текстом ROLE или отправь: @username ROLE\n\nmoderator: сообщения, участники, темы\nadmin: дополнительно оформление и закрепление\nowner: дополнительно назначение администраторов\nmember: снять админку", false)
		}
	case "admwarn":
		if b.canModerate(u) {
			b.startAdminSession(u, q, stepAdminWarn, "Ответь на сообщение пользователя или отправь @username / Telegram ID.", false)
		}
	case "admtag":
		if b.canModerate(u) {
			b.startAdminSession(u, q, stepAdminTag, "Ответь тегами на сообщение пользователя или отправь: @username TAGS", false)
		}
	case "admcmd":
		if b.canModerate(u) {
			b.startAdminSession(u, q, stepAdminCommand, "Отправь: NAME ТЕКСТ. Переменные: {name}, {username}, {id}", false)
		}
	case "admblock":
		if b.canModerate(u) {
			b.startAdminSession(u, q, stepAdminBlock, "Ответь на сообщение пользователя или отправь @username / Telegram ID.", false)
		}
	case "admunblock":
		if b.canModerate(u) {
			b.startAdminSession(u, q, stepAdminUnblock, "Ответь на сообщение пользователя или отправь @username / Telegram ID.", false)
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
	target, ok := b.targetForLanguage(ctx, p.Language)
	if !ok {
		b.edit(q, "Для этого языка не настроена группа.", nil)
		_ = b.store.DeleteProject(ctx, p.ID)
		return
	}
	sent, err := b.publishMessage(target, formatPublishedProject(p, author, target.ChatUsername))
	if err != nil {
		_ = b.store.DeleteProject(ctx, p.ID)
		b.logErr(err)
		b.edit(q, "Публикация не удалась. Проверь настройки группы или попробуй позже.", nil)
		return
	}
	_ = b.store.SetPublication(ctx, p.ID, sent.Chat.ID, sent.MessageID)
	p.PublishedChatID, p.PublishedMessageID = sent.Chat.ID, sent.MessageID
	channelPublished := false
	if b.cfg.ProjectsChannel.ChatID != 0 || b.cfg.ProjectsChannel.Username != "" {
		channel, channelErr := b.publishChannelMessage(formatChannelProject(p, b.groupMessageURL(p)))
		if channelErr != nil {
			b.logErr(channelErr)
		} else {
			channelPublished = true
			p.ChannelChatID, p.ChannelMessageID = channel.Chat.ID, channel.MessageID
			_ = b.store.SetChannelPublication(ctx, p.ID, channel.Chat.ID, channel.MessageID)
		}
	}
	b.sessions.delete(u.TelegramID)
	if b.cfg.ProjectsChannel.ChatID != 0 || b.cfg.ProjectsChannel.Username != "" {
		if !channelPublished {
			b.edit(q, "Проект опубликован в языковой группе, но отправить его в общий канал не удалось.", nil)
			return
		}
	}
	b.edit(q, "Готово! Проект опубликован в языковой группе и общей ленте.", nil)
}

func (b *Bot) projectFilters(chatID int64) {
	kb := starsKeyboard()
	b.send(chatID, "<b>Каталог проектов</b>\n\nСколько звёзд должно быть у репозитория?", kb)
}

func starsKeyboard() *tgbotapi.InlineKeyboardMarkup {
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Любое", "stars:0"), tgbotapi.NewInlineKeyboardButtonData("⭐ 1+", "stars:1"), tgbotapi.NewInlineKeyboardButtonData("⭐ 10+", "stars:10")),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("⭐ 50+", "stars:50"), tgbotapi.NewInlineKeyboardButtonData("⭐ 100+", "stars:100"), tgbotapi.NewInlineKeyboardButtonData("⭐ 500+", "stars:500")),
	)
	return &kb
}
func (b *Bot) projectFiltersEdit(q *tgbotapi.CallbackQuery) {
	b.edit(q, "<b>Каталог проектов</b>\n\nСколько звёзд должно быть у репозитория?", starsKeyboard())
}
func (b *Bot) contributorsFilter(q *tgbotapi.CallbackQuery, stars string) {
	if _, err := strconv.Atoi(stars); err != nil {
		return
	}
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Все проекты", "catalog:"+stars+",0,0"), tgbotapi.NewInlineKeyboardButtonData("Нужны участники", "catalog:"+stars+",1,0")),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("← Назад", "filters:menu")),
	)
	b.edit(q, "Показывать только проекты, которым нужны контрибьюторы?", &kb)
}

const catalogPageSize = 3

func (b *Bot) showProjects(ctx context.Context, chatID int64, minStars int, contributors bool, offset int, q *tgbotapi.CallbackQuery) {
	items, err := b.store.ListProjects(ctx, minStars, contributors, catalogPageSize+1, offset)
	if err != nil {
		b.logErr(err)
		return
	}
	text := "Проекты не найдены."
	hasNext := len(items) > catalogPageSize
	if hasNext {
		items = items[:catalogPageSize]
	}
	if len(items) > 0 {
		var parts []string
		for _, p := range items {
			parts = append(parts, b.formatCatalogProject(p))
		}
		text = strings.Join(parts, "\n\n──────────\n\n")
	}
	var row []tgbotapi.InlineKeyboardButton
	flag := 0
	if contributors {
		flag = 1
	}
	if offset > 0 {
		row = append(row, tgbotapi.NewInlineKeyboardButtonData("←", fmt.Sprintf("catalog:%d,%d,%d", minStars, flag, max(0, offset-catalogPageSize))))
	}
	if hasNext {
		row = append(row, tgbotapi.NewInlineKeyboardButtonData("→", fmt.Sprintf("catalog:%d,%d,%d", minStars, flag, offset+catalogPageSize)))
	}
	rows := [][]tgbotapi.InlineKeyboardButton{tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("⚙️ Изменить фильтр", "filters:menu"))}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	k := tgbotapi.NewInlineKeyboardMarkup(rows...)
	kb := &k
	if q != nil {
		b.edit(q, text, kb)
	} else {
		b.send(chatID, text, kb)
	}
}

func catalogParams(value string) (int, bool, int, bool) {
	parts := strings.Split(value, ",")
	if len(parts) != 3 {
		return 0, false, 0, false
	}
	stars, e1 := strconv.Atoi(parts[0])
	flag, e2 := strconv.Atoi(parts[1])
	offset, e3 := strconv.Atoi(parts[2])
	valid := e1 == nil && e2 == nil && e3 == nil && stars >= 0 && (flag == 0 || flag == 1) && offset >= 0
	return stars, flag == 1, offset, valid
}

func (b *Bot) publishMessage(target config.ProjectTarget, text string) (tgbotapi.Message, error) {
	chat := target.ChatUsername
	if target.ChatID != 0 {
		chat = strconv.FormatInt(target.ChatID, 10)
	}
	response, err := b.api.MakeRequest("sendMessage", tgbotapi.Params{
		"chat_id": chat, "message_thread_id": strconv.Itoa(target.ThreadID),
		"text": text, "parse_mode": "HTML", "disable_web_page_preview": "true",
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

func (b *Bot) publishChannelMessage(text string) (tgbotapi.Message, error) {
	chat := b.cfg.ProjectsChannel.Username
	if b.cfg.ProjectsChannel.ChatID != 0 {
		chat = strconv.FormatInt(b.cfg.ProjectsChannel.ChatID, 10)
	}
	response, err := b.api.MakeRequest("sendMessage", tgbotapi.Params{"chat_id": chat, "text": text, "parse_mode": "HTML", "disable_web_page_preview": "true"})
	if err != nil {
		return tgbotapi.Message{}, err
	}
	var message tgbotapi.Message
	if err = json.Unmarshal(response.Result, &message); err != nil {
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
		"text": text, "parse_mode": "HTML", "disable_web_page_preview": "true",
	})
	return err
}

func (b *Bot) editChannelMessage(p domain.Project, text string) error {
	if p.ChannelChatID == 0 || p.ChannelMessageID == 0 {
		return nil
	}
	_, err := b.api.MakeRequest("editMessageText", tgbotapi.Params{"chat_id": strconv.FormatInt(p.ChannelChatID, 10), "message_id": strconv.Itoa(p.ChannelMessageID), "text": text, "parse_mode": "HTML", "disable_web_page_preview": "true"})
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
	closed := fmt.Sprintf("<b>%s</b> автор (имя: %s) решил исключить проект из каталога сообщества.", esc(p.Name), esc(u.Username))
	if err = b.editPublishedMessage(p, closed); err != nil {
		b.logErr(err)
		b.edit(q, "Проект закрыт в каталоге, но сообщение в группе обновить не удалось.", nil)
		return
	}
	if err = b.editChannelMessage(p, closed); err != nil {
		b.logErr(err)
		b.edit(q, "Проект закрыт, но сообщение в общем канале обновить не удалось.", nil)
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
	target, _ := b.targetForLanguage(ctx, p.Language)
	if err = b.editPublishedMessage(p, formatPublishedProject(p, author, target.ChatUsername)); err != nil {
		b.logErr(err)
	}
	if b.cfg.ProjectsChannel.ChatID != 0 || b.cfg.ProjectsChannel.Username != "" {
		if p.ChannelMessageID == 0 {
			message, publishErr := b.publishChannelMessage(formatChannelProject(p, b.groupMessageURL(p)))
			if publishErr != nil {
				b.logErr(publishErr)
			} else {
				_ = b.store.SetChannelPublication(ctx, p.ID, message.Chat.ID, message.MessageID)
			}
		} else if err = b.editChannelMessage(p, formatChannelProject(p, b.groupMessageURL(p))); err != nil {
			b.logErr(err)
		}
	}
}

func repoProject(current domain.Project, repo gh.Repo) domain.Project {
	current.RepoURL, current.RepoOwner, current.RepoName, current.Description = repo.URL, repo.Owner, repo.Name, repo.Description
	current.Topics, current.Stars = strings.Join(repo.Topics, ","), strconv.Itoa(repo.Stars)
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

func (b *Bot) groupCommand(ctx context.Context, u domain.User, m *tgbotapi.Message) {
	if !m.IsCommand() {
		return
	}
	if m.Command() == "admin" {
		_, _ = b.api.Request(tgbotapi.NewDeleteMessage(m.Chat.ID, m.MessageID))
		if !b.canModerate(u) {
			return
		}
	}
	if m.Command() == "info" {
		b.showRequestedUserInfo(ctx, u, m)
		return
	}
	if m.Command() == "ai" {
		b.startAIReview(ctx, u, m)
		return
	}
	if !b.canModerate(u) {
		b.runCustomCommand(ctx, m)
		return
	}
	args := strings.Fields(m.CommandArguments())
	switch m.Command() {
	case "admin":
		b.adminAction(ctx, u, m, args)
	case "stats":
		x, e := b.store.Stats(ctx)
		if e == nil {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, fmt.Sprintf("Пользователей: %d\nПроектов: %d\nИщут участников: %d", x.Users, x.Projects, x.Contributors), nil)
		}
	case "hideproject":
		b.adminProjectStatus(ctx, m, args, domain.StatusHidden)
	case "showproject":
		b.adminProjectStatus(ctx, m, args, domain.StatusPublished)
	case "block":
		b.adminBlock(ctx, m, args, true)
	case "unblock":
		b.adminBlock(ctx, m, args, false)
	default:
		b.runCustomCommand(ctx, m)
	}
}

func (b *Bot) startAIReview(ctx context.Context, u domain.User, m *tgbotapi.Message) {
	repoURL := extractGitHubURL(m.CommandArguments())
	if repoURL == "" && m.ReplyToMessage != nil {
		repoURL = githubURLFromMessage(m.ReplyToMessage)
	}
	if repoURL == "" {
		b.sendInThread(m.Chat.ID, m.MessageThreadID, "Ответь на сообщение со ссылкой GitHub или отправь: /ai https://github.com/owner/repo", nil)
		return
	}
	models, err := b.reviewer.AvailableModels(ctx)
	if err != nil {
		b.sendInThread(m.Chat.ID, m.MessageThreadID, "Локальная ollama недоступна: "+esc(err.Error()), nil)
		return
	}
	if len(models) == 0 {
		b.sendInThread(m.Chat.ID, m.MessageThreadID, "В ollama нет доступных моделей.", nil)
		return
	}
	b.sessions.set(u.TelegramID, session{Step: stepAIModel, AIRepoURL: repoURL})
	kb := aiModelKeyboard(models)
	b.sendOwnedInThread(m.Chat.ID, m.MessageThreadID, "Выбери модель для обзора проекта:", &kb, u.TelegramID)
}

func aiModelKeyboard(models []aireview.Model) tgbotapi.InlineKeyboardMarkup {
	var rows [][]tgbotapi.InlineKeyboardButton
	var row []tgbotapi.InlineKeyboardButton
	for i, model := range models {
		label := model.Label
		if label == "" {
			label = model.Name
		}
		row = append(row, tgbotapi.NewInlineKeyboardButtonData(label, "aimodel:"+model.Name))
		if len(row) == 2 || i == len(models)-1 {
			rows = append(rows, row)
			row = nil
		}
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Отмена", "aicancel:run")))
	return tgbotapi.NewInlineKeyboardMarkup(rows...)
}

func githubURLFromMessage(m *tgbotapi.Message) string {
	if m == nil {
		return ""
	}
	for _, value := range []string{m.Text, m.Caption} {
		if url := extractGitHubURL(value); url != "" {
			return url
		}
	}
	return ""
}

func extractGitHubURL(text string) string {
	for _, part := range strings.Fields(strings.TrimSpace(text)) {
		part = strings.Trim(part, " \n\r\t<>()[]{}\",")
		if strings.HasPrefix(strings.ToLower(part), "https://github.com/") || strings.HasPrefix(strings.ToLower(part), "http://github.com/") {
			return part
		}
	}
	return ""
}

func (b *Bot) runAIReview(ctx context.Context, chatID int64, threadID int, repoURL, model string) {
	statusMsg := tgbotapi.NewMessage(chatID, "<b>AI-обзор</b>\nПодготовка анализа…")
	statusMsg.MessageThreadID = threadID
	statusMsg.ParseMode = "HTML"
	sent, err := b.api.Send(statusMsg)
	if err != nil {
		b.logErr(err)
		return
	}
	reviewCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	started := time.Now()
	lastText := ""
	lastPush := time.Time{}
	push := func(text string, force bool) {
		if text == "" {
			return
		}
		if !force && time.Since(lastPush) < 1200*time.Millisecond {
			return
		}
		if text == lastText {
			return
		}
		lastText = text
		lastPush = time.Now()
		b.editMessage(sent.Chat.ID, sent.MessageID, text)
	}
	header := "<b>AI-обзор проекта</b>\nМодель: <code>" + esc(model) + "</code>\nРепозиторий: " + esc(repoURL)
	result, err := b.reviewer.ReviewStream(reviewCtx, repoURL, model, func(p aireview.Progress) error {
		body := p.Status
		if strings.TrimSpace(p.Text) != "" {
			body = "Поток ответа:\n\n" + esc(p.Text)
		}
		if body == "" {
			body = "Подготовка анализа…"
		}
		push(header+"\n\n"+body, p.Done)
		return nil
	})
	if err != nil {
		b.editMessage(sent.Chat.ID, sent.MessageID, header+"\n\nНе удалось сделать обзор: "+esc(err.Error()))
		return
	}
	finalText := header + "\n\n" + esc(result.Text) + "\n\n<i>Готово за " + esc(time.Since(started).Round(time.Second).String()) + ".</i>"
	push(finalText, true)
}

func (b *Bot) sendLongHTML(chatID int64, threadID int, text string) {
	const limit = 3800
	for len(text) > 0 {
		chunk := text
		if len(chunk) > limit {
			split := strings.LastIndex(chunk[:limit], "\n")
			if split < limit/2 {
				split = limit
			}
			chunk = chunk[:split]
		}
		b.sendInThread(chatID, threadID, chunk, nil)
		text = strings.TrimLeft(text[len(chunk):], "\n")
	}
}

func (b *Bot) editMessage(chatID int64, messageID int, text string) {
	m := tgbotapi.NewEditMessageText(chatID, messageID, text)
	m.ParseMode = "HTML"
	m.LinkPreviewOptions = tgbotapi.LinkPreviewOptions{IsDisabled: true}
	if _, err := b.api.Send(m); err != nil {
		b.logErr(err)
	}
}

func (b *Bot) adminProjectStatus(ctx context.Context, m *tgbotapi.Message, args []string, status string) {
	if len(args) != 1 {
		b.sendInThread(m.Chat.ID, m.MessageThreadID, "Укажи ID проекта.", nil)
		return
	}
	id, e := strconv.ParseInt(args[0], 10, 64)
	if e != nil {
		b.sendInThread(m.Chat.ID, m.MessageThreadID, "Некорректный ID.", nil)
		return
	}
	if e = b.store.SetProjectStatus(ctx, id, status); e != nil {
		b.sendInThread(m.Chat.ID, m.MessageThreadID, e.Error(), nil)
		return
	}
	b.sendInThread(m.Chat.ID, m.MessageThreadID, "Статус проекта обновлён.", nil)
}
func (b *Bot) adminBlock(ctx context.Context, m *tgbotapi.Message, args []string, blocked bool) {
	reference := ""
	if len(args) > 0 {
		reference = args[0]
	}
	target, err := b.resolveUserReference(ctx, m, reference)
	if err != nil {
		b.sendInThread(m.Chat.ID, m.MessageThreadID, "Ответь командой на сообщение или укажи @username / ID.", nil)
		return
	}
	failures, err := b.setNetworkBlock(ctx, target.TelegramID, blocked)
	if err != nil {
		b.sendInThread(m.Chat.ID, m.MessageThreadID, "Не удалось обновить блокировку.", nil)
		return
	}
	if len(failures) > 0 {
		b.sendInThread(m.Chat.ID, m.MessageThreadID, "Статус обновлён, ошибки в группах: "+esc(strings.Join(failures, ", ")), nil)
		return
	}
	if blocked {
		b.sendInThread(m.Chat.ID, m.MessageThreadID, "Пользователь заблокирован во всей сети.", nil)
	} else {
		b.sendInThread(m.Chat.ID, m.MessageThreadID, "Пользователь разблокирован во всей сети.", nil)
	}
}
func (b *Bot) adminHelp(chatID int64) {
	b.send(chatID, "Админ-команды (работают и в группе):\n/stats\n/hideproject ID\n/showproject ID\n/block TELEGRAM_ID\n/unblock TELEGRAM_ID", nil)
}
func (b *Bot) canModerate(u domain.User) bool {
	if b.isOwner(u) {
		return true
	}
	if u.Role == "admin" || u.Role == "moderator" {
		return true
	}
	_, ok := b.cfg.AdminIDs[u.TelegramID]
	return ok
}
func (b *Bot) isOwner(u domain.User) bool {
	return u.Role == "owner" || b.cfg.NetworkOwnerID != 0 && u.TelegramID == b.cfg.NetworkOwnerID
}
func (b *Bot) isAdmin(id int64) bool { _, ok := b.cfg.AdminIDs[id]; return ok }

func (b *Bot) adminAction(ctx context.Context, u domain.User, m *tgbotapi.Message, args []string) {
	if len(args) == 0 {
		b.adminMenu(ctx, u, m.Chat.ID, m.MessageThreadID)
		return
	}
	switch args[0] {
	case "add":
		if !b.isOwner(u) {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Только владелец сети может добавлять группы.", nil)
			return
		}
		if len(args) != 3 || m.MessageThreadID == 0 {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Выполни команду в нужной теме: /admin add NAME LANG", nil)
			return
		}
		if _, err := b.store.RegisteredGroupByChat(ctx, m.Chat.ID); err != nil {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Сначала зарегистрируй группу: /admin register NAME", nil)
			return
		}
		g := domain.NetworkGroup{Name: args[1], Language: args[2], ChatID: m.Chat.ID, ChatUsername: m.Chat.UserName, ThreadID: m.MessageThreadID}
		if err := b.store.UpsertNetworkGroup(ctx, g); err != nil {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Не удалось сохранить группу: "+esc(err.Error()), nil)
		} else {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Группа и тема сохранены в сети.", nil)
		}
	case "register":
		if !b.isOwner(u) || len(args) != 2 {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Формат для владельца: /admin register NAME", nil)
			return
		}
		missing, err := b.missingBotRights(m.Chat.ID)
		if err != nil {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Не удалось проверить права бота.", nil)
			return
		}
		if len(missing) > 0 {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Не хватает прав: "+esc(strings.Join(missing, ", "))+". Бот выходит из группы.", nil)
			_, _ = b.api.MakeRequest("leaveChat", tgbotapi.Params{"chat_id": strconv.FormatInt(m.Chat.ID, 10)})
			return
		}
		if err = b.store.RegisterGroup(ctx, domain.RegisteredGroup{Name: args[1], ChatID: m.Chat.ID, ChatUsername: m.Chat.UserName}, u.TelegramID); err != nil {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Не удалось зарегистрировать группу.", nil)
		} else {
			b.syncBlockedUsers(ctx, m.Chat.ID)
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Группа зарегистрирована как участник сети.", nil)
		}
	case "syncadmins":
		if !b.isOwner(u) {
			return
		}
		failures := b.syncNetworkAdmins(ctx)
		if len(failures) > 0 {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Синхронизация выполнена по всей сети. Ошибки:\n"+esc(strings.Join(failures, "\n")), nil)
		} else {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Роли синхронизированы во всей сети.", nil)
		}
	case "sanction":
		if !b.isOwner(u) {
			return
		}
		kb := tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Подтвердить исключение", fmt.Sprintf("admsanction:%d", m.Chat.ID)), tgbotapi.NewInlineKeyboardButtonData("Отмена", "admmenu:show")))
		b.sendOwnedInThread(m.Chat.ID, m.MessageThreadID, "Исключить группу из сети и вывести бота? Участники и история останутся без изменений.", &kb, u.TelegramID)
	case "antispam":
		enabled, err := b.store.ToggleAntiSpam(ctx, m.Chat.ID)
		if err != nil {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Сначала добавь группу в сеть.", nil)
		} else {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, fmt.Sprintf("Антиспам: %s", map[bool]string{true: "включён", false: "выключен"}[enabled]), nil)
		}
	case "role", "appoint":
		if !b.isOwner(u) {
			return
		}
		reference, role := "", ""
		if m.ReplyToMessage != nil && len(args) == 2 {
			role = args[1]
		} else if len(args) == 3 {
			reference, role = args[1], args[2]
		} else {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Reply: /admin role ROLE; либо /admin role @username ROLE", nil)
			return
		}
		valid := map[string]bool{"owner": true, "admin": true, "moderator": true, "member": true}
		if !valid[role] {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Некорректная роль.", nil)
			return
		}
		target, e := b.resolveUserReference(ctx, m, reference)
		if e != nil {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Пользователь не найден.", nil)
			return
		}
		if e = b.store.SetRole(ctx, target.TelegramID, role); e != nil {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Пользователь сначала должен открыть бота.", nil)
		} else {
			target.Role = role
			failures := b.syncUserRole(ctx, target)
			if len(failures) > 0 {
				b.sendInThread(m.Chat.ID, m.MessageThreadID, "Роль сохранена, Telegram отказал: "+esc(strings.Join(failures, ", ")), nil)
			} else {
				b.sendInThread(m.Chat.ID, m.MessageThreadID, "Роль обновлена во всей сети.", nil)
			}
		}
	case "warn":
		reference := ""
		if len(args) > 1 {
			reference = args[1]
		}
		target, err := b.resolveUserReference(ctx, m, reference)
		if err != nil {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Reply или @username / ID.", nil)
			return
		}
		if err := b.store.AddWarn(ctx, target.TelegramID); err != nil {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Пользователь не найден.", nil)
		} else {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Варн добавлен.", nil)
		}
	case "tag":
		reference, tags := "", ""
		if m.ReplyToMessage != nil && len(args) >= 2 {
			tags = strings.Join(args[1:], ",")
		} else if len(args) >= 3 {
			reference, tags = args[1], strings.Join(args[2:], ",")
		} else {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Reply: /admin tag TAGS; либо /admin tag @username TAGS", nil)
			return
		}
		target, err := b.resolveUserReference(ctx, m, reference)
		if err != nil {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Пользователь не найден.", nil)
			return
		}
		if err := b.store.SetTags(ctx, target.TelegramID, tags); err != nil {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Пользователь не найден.", nil)
		} else {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Теги обновлены.", nil)
		}
	case "newcmd":
		if len(args) < 3 {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Формат: /admin newcmd NAME ТЕКСТ. Переменные: {name}, {username}, {id}", nil)
			return
		}
		name := strings.TrimPrefix(strings.ToLower(args[1]), "/")
		if err := b.store.SaveCustomCommand(ctx, name, strings.Join(args[2:], " "), u.TelegramID); err != nil {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Не удалось сохранить команду.", nil)
		} else {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Команда /"+esc(name)+" сохранена.", nil)
		}
	case "new":
		if len(args) < 5 || args[1] != "cmd" || (args[3] != "reply" && args[3] != "ответ") {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Формат: /admin new cmd NAME reply ТЕКСТ", nil)
			return
		}
		name := strings.TrimPrefix(strings.ToLower(args[2]), "/")
		if err := b.store.SaveCustomCommand(ctx, name, strings.Join(args[4:], " "), u.TelegramID); err != nil {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Не удалось сохранить команду.", nil)
		} else {
			b.sendInThread(m.Chat.ID, m.MessageThreadID, "Команда /"+esc(name)+" сохранена.", nil)
		}
	default:
		b.adminMenu(ctx, u, m.Chat.ID, m.MessageThreadID)
	}
}

func (b *Bot) adminMenu(ctx context.Context, u domain.User, chatID int64, threadID int) {
	kb := b.adminKeyboard(u)
	b.sendOwnedInThread(chatID, threadID, "<b>Управление сетью</b>", &kb, u.TelegramID)
}
func (b *Bot) adminMenuEdit(ctx context.Context, u domain.User, q *tgbotapi.CallbackQuery) {
	b.bindButtons(q.Message.Chat.ID, q.Message.MessageID, u.TelegramID)
	kb := b.adminKeyboard(u)
	b.edit(q, "<b>Управление сетью</b>", &kb)
}
func (b *Bot) adminKeyboard(u domain.User) tgbotapi.InlineKeyboardMarkup {
	rows := [][]tgbotapi.InlineKeyboardButton{tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Сеть", "admcat:network"), tgbotapi.NewInlineKeyboardButtonData("Модерация", "admcat:moderation")), tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Пользователи", "admcat:users"), tgbotapi.NewInlineKeyboardButtonData("Команды", "admcat:commands")), tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Статистика", "admstats:show"))}
	return tgbotapi.NewInlineKeyboardMarkup(rows...)
}
func (b *Bot) adminCategory(u domain.User, q *tgbotapi.CallbackQuery, category string) {
	back := tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Назад", "admmenu:show"))
	var rows [][]tgbotapi.InlineKeyboardButton
	title := ""
	switch category {
	case "network":
		title = "Сеть"
		if b.isOwner(u) {
			rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Регистрация группы", "admregister:start"), tgbotapi.NewInlineKeyboardButtonData("Регистрация канала", "admchannel:start")), tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Тема проектов", "admadd:start"), tgbotapi.NewInlineKeyboardButtonData("Участники сети", "admgroup:list")), tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Синхронизация админов", "admsync:run")), tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Исключить группу", "admsanctionask:show")))
			rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Массовая рассылка", "admbroadcast:show"), tgbotapi.NewInlineKeyboardButtonData("Настройки сети", "admsettings:show")))
		}
	case "moderation":
		title = "Модерация"
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Антиспам", "admantispam:show"), tgbotapi.NewInlineKeyboardButtonData("Добавить варн", "admwarn:start")))
	case "users":
		title = "Пользователи"
		if b.isOwner(u) {
			rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Назначить администратора", "admrole:start")))
		}
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Назначить теги", "admtag:start")))
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Блокировать", "admblock:start"), tgbotapi.NewInlineKeyboardButtonData("Разблокировать", "admunblock:start")))
	case "commands":
		title = "Команды"
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Новая команда", "admcmd:start"), tgbotapi.NewInlineKeyboardButtonData("События", "admevents:show")))
	default:
		b.adminMenuEdit(context.Background(), u, q)
		return
	}
	rows = append(rows, back)
	kb := tgbotapi.NewInlineKeyboardMarkup(rows...)
	b.edit(q, "<b>"+title+"</b>", &kb)
}

func (b *Bot) antiSpamMenu(ctx context.Context, q *tgbotapi.CallbackQuery) {
	g, err := b.store.NetworkGroupByChat(ctx, q.Message.Chat.ID)
	if err != nil {
		b.edit(q, "Сначала зарегистрируй тему проектов этой группы.", b.adminBackKeyboard())
		return
	}
	status := map[bool]string{true: "включён", false: "выключен"}[g.AntiSpam]
	action := map[string]string{"delete": "удалять", "delete_warn": "удалять и добавлять варн"}[g.SpamAction]
	kb := tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Включить / выключить", "antitoggle:run")), tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("3", "antilimit:3"), tgbotapi.NewInlineKeyboardButtonData("6", "antilimit:6"), tgbotapi.NewInlineKeyboardButtonData("10", "antilimit:10"), tgbotapi.NewInlineKeyboardButtonData("15", "antilimit:15")), tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("5 сек", "antiwindow:5"), tgbotapi.NewInlineKeyboardButtonData("10 сек", "antiwindow:10"), tgbotapi.NewInlineKeyboardButtonData("30 сек", "antiwindow:30"), tgbotapi.NewInlineKeyboardButtonData("60 сек", "antiwindow:60")), tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Только удалить", "antiaction:delete"), tgbotapi.NewInlineKeyboardButtonData("Удалить и варн", "antiaction:delete_warn")), tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Назад", "admcat:moderation")))
	b.edit(q, fmt.Sprintf("<b>Антиспам</b>\nСтатус: %s\nЛимит: %d сообщений\nОкно: %d секунд\nДействие: %s", status, g.SpamLimit, g.SpamWindow, action), &kb)
}
func (b *Bot) startAdminSession(u domain.User, q *tgbotapi.CallbackQuery, next step, prompt string, needTopic bool) {
	if q.Message.Chat.Type == "private" && next == stepAdminAddRoute {
		b.edit(q, "Эта операция запускается через /admin внутри группы.", b.adminBackKeyboard())
		return
	}
	if needTopic && q.Message.MessageThreadID == 0 {
		b.edit(q, "Открой меню /admin внутри нужной темы проектов.", b.adminBackKeyboard())
		return
	}
	b.sessions.set(u.TelegramID, session{Step: next, AdminChatID: q.Message.Chat.ID, AdminThreadID: q.Message.MessageThreadID, AdminChatUsername: q.Message.Chat.UserName})
	b.edit(q, prompt, b.adminBackKeyboard())
}
func (b *Bot) registerGroupFromButton(ctx context.Context, u domain.User, q *tgbotapi.CallbackQuery) {
	chat := q.Message.Chat
	if chat.Type == "private" {
		b.edit(q, "Открой /admin внутри группы.", b.adminBackKeyboard())
		return
	}
	missing, err := b.missingBotRights(chat.ID)
	if err != nil {
		b.edit(q, "Не удалось проверить права бота.", b.adminBackKeyboard())
		return
	}
	if len(missing) > 0 {
		b.edit(q, "Не хватает прав: "+esc(strings.Join(missing, ", "))+". Бот выходит из группы.", nil)
		_, _ = b.api.MakeRequest("leaveChat", tgbotapi.Params{"chat_id": strconv.FormatInt(chat.ID, 10)})
		return
	}
	name := strings.TrimSpace(chat.Title)
	if name == "" {
		name = strings.TrimPrefix(chat.UserName, "@")
	}
	if err = b.store.RegisterGroup(ctx, domain.RegisteredGroup{Name: name, ChatID: chat.ID, ChatUsername: chat.UserName}, u.TelegramID); err != nil {
		b.edit(q, "Не удалось зарегистрировать группу.", b.adminBackKeyboard())
	} else {
		b.syncBlockedUsers(ctx, chat.ID)
		b.edit(q, "Группа \""+esc(name)+"\" зарегистрирована.", b.adminBackKeyboard())
	}
}
func (b *Bot) deleteAdminInput(m *tgbotapi.Message) {
	if m.Chat.Type != "private" {
		_, _ = b.api.Request(tgbotapi.NewDeleteMessage(m.Chat.ID, m.MessageID))
	}
}
func (b *Bot) adminBackKeyboard() *tgbotapi.InlineKeyboardMarkup {
	kb := tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Назад", "admmenu:show")))
	return &kb
}
func (b *Bot) broadcastScopeMenu(q *tgbotapi.CallbackQuery) {
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Вся сеть", "admbroadcastscope:all")),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Только группы", "admbroadcastscope:groups"), tgbotapi.NewInlineKeyboardButtonData("Только каналы", "admbroadcastscope:channels")),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Назад", "admcat:network")),
	)
	b.edit(q, "<b>Массовая рассылка</b>\n\nВыбери получателей. Сообщения рассылки не удаляются автоматически.", &kb)
}
func validBroadcastScope(scope string) bool {
	return scope == "all" || scope == "groups" || scope == "channels"
}
func broadcastScopeName(scope string) string {
	return map[string]string{"all": "вся сеть", "groups": "только группы", "channels": "только каналы"}[scope]
}
func validateBroadcastHTML(text string) error {
	decoder := xml.NewDecoder(strings.NewReader("<root>" + text + "</root>"))
	allowed := map[string]bool{"root": true, "b": true, "strong": true, "i": true, "em": true, "u": true, "ins": true, "s": true, "strike": true, "del": true, "code": true, "pre": true, "blockquote": true, "tg-spoiler": true, "span": true, "a": true, "tg-emoji": true}
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("теги не закрыты или символ & не экранирован")
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		name := start.Name.Local
		if !allowed[name] {
			return fmt.Errorf("тег <%s> не поддерживается", name)
		}
		for _, attr := range start.Attr {
			if !validBroadcastAttribute(name, attr.Name.Local, attr.Value) {
				return fmt.Errorf("атрибут %s у тега <%s> не поддерживается", attr.Name.Local, name)
			}
		}
	}
}
func looksLikeBroadcastHTML(text string) bool {
	lower := strings.ToLower(text)
	for _, tag := range []string{"b", "strong", "i", "em", "u", "ins", "s", "strike", "del", "code", "pre", "blockquote", "tg-spoiler", "span", "a", "tg-emoji"} {
		if strings.Contains(lower, "<"+tag+">") || strings.Contains(lower, "<"+tag+" ") || strings.Contains(lower, "</"+tag+">") {
			return true
		}
	}
	return false
}
func validBroadcastAttribute(tag, name, value string) bool {
	switch tag {
	case "a":
		if name != "href" {
			return false
		}
		u, err := url.Parse(strings.TrimSpace(value))
		return err == nil && (u.Scheme == "https" || u.Scheme == "http" || u.Scheme == "tg" || u.Scheme == "mailto")
	case "span":
		return name == "class" && value == "tg-spoiler"
	case "code":
		return name == "class" && strings.HasPrefix(value, "language-") && len(value) > len("language-")
	case "blockquote":
		return name == "expandable" && (value == "" || value == "expandable" || value == "true")
	case "tg-emoji":
		if name != "emoji-id" {
			return false
		}
		_, err := strconv.ParseInt(value, 10, 64)
		return err == nil
	default:
		return false
	}
}
func (b *Bot) sendBroadcast(ctx context.Context, s session) (int, []string) {
	chats, err := b.store.RegisteredGroups(ctx)
	if err != nil {
		return 0, []string{err.Error()}
	}
	sent := 0
	var failures []string
	for _, chat := range chats {
		isChannel := chat.ChatType == "channel"
		if s.BroadcastScope == "groups" && isChannel || s.BroadcastScope == "channels" && !isChannel {
			continue
		}
		var sendErr error
		if s.BroadcastMode == "copy" {
			copyMessage := tgbotapi.NewCopyMessage(chat.ChatID, s.BroadcastChatID, s.BroadcastMessageID)
			_, sendErr = b.api.Request(copyMessage)
		} else {
			msg := tgbotapi.NewMessage(chat.ChatID, s.BroadcastText)
			msg.ParseMode = "HTML"
			msg.LinkPreviewOptions = tgbotapi.LinkPreviewOptions{IsDisabled: true}
			_, sendErr = b.api.Send(msg)
		}
		if sendErr != nil {
			failures = append(failures, chat.Name+": "+sendErr.Error())
			b.logErr(fmt.Errorf("broadcast to %s: %w", chat.Name, sendErr))
			continue
		}
		sent++
	}
	return sent, failures
}
func (b *Bot) networkSettings(ctx context.Context, q *tgbotapi.CallbackQuery) {
	chats, _ := b.store.RegisteredGroups(ctx)
	groups, channels := 0, 0
	for _, chat := range chats {
		if chat.ChatType == "channel" {
			channels++
		} else {
			groups++
		}
	}
	seconds := b.store.NetworkSetting(ctx, "service_delete_seconds", "30")
	deleteText := seconds + " сек"
	if seconds == "0" {
		deleteText = "выключено"
	}
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Не удалять", "admautodelete:0"), tgbotapi.NewInlineKeyboardButtonData("15 сек", "admautodelete:15"), tgbotapi.NewInlineKeyboardButtonData("30 сек", "admautodelete:30")),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("60 сек", "admautodelete:60"), tgbotapi.NewInlineKeyboardButtonData("120 сек", "admautodelete:120")),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Назад", "admcat:network")),
	)
	b.edit(q, fmt.Sprintf("<b>Настройки сети</b>\n\nГрупп: %d\nКаналов: %d\nАвтоудаление служебных сообщений: %s", groups, channels, deleteText), &kb)
}
func (b *Bot) automationScopeMenu(u domain.User, q *tgbotapi.CallbackQuery) {
	rows := [][]tgbotapi.InlineKeyboardButton{}
	if q.Message.Chat.Type != "private" && q.Message.Chat.Type != "channel" {
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Локальные для группы", "admeventscope:local")))
	}
	if b.isOwner(u) {
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Глобальные для сети", "admeventscope:global")))
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Назад", "admcat:commands")))
	kb := tgbotapi.NewInlineKeyboardMarkup(rows...)
	b.edit(q, "<b>Область событий</b>\n\nЛокальные работают только в одной группе. Глобальные работают во всех группах сети.", &kb)
}
func (b *Bot) automationMenu(ctx context.Context, u domain.User, q *tgbotapi.CallbackQuery, scope string) {
	chatID := q.Message.Chat.ID
	if scope == "global" {
		if !b.isOwner(u) {
			return
		}
		chatID = 0
	} else {
		if q.Message.Chat.Type == "private" || q.Message.Chat.Type == "channel" {
			b.edit(q, "Локальные события настраиваются внутри нужной группы.", b.adminBackKeyboard())
			return
		}
		if _, err := b.store.RegisteredGroupByChat(ctx, chatID); err != nil {
			b.edit(q, "Сначала зарегистрируй группу в сети.", b.adminBackKeyboard())
			return
		}
	}
	rules, err := b.store.AutomationRules(ctx, chatID, "")
	if err != nil {
		b.edit(q, "Не удалось загрузить события.", b.adminBackKeyboard())
		return
	}
	events := map[string]string{"join": "Вход", "leave": "Выход", "message": "Сообщение", "command": "Команда", "photo": "Фото", "file": "Файл", "link": "Ссылка", "antispam": "Антиспам", "warns": "Порог варнов"}
	actions := map[string]string{"send": "отправить текст", "delete": "удалить сообщение", "warn": "добавить варн", "ban": "бан во всей сети"}
	title := "Локальные события группы"
	if scope == "global" {
		title = "Глобальные события сети"
	}
	text := "<b>" + title + "</b>"
	rows := [][]tgbotapi.InlineKeyboardButton{tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Добавить событие", "admevent:"+scope+",choose"))}
	for _, r := range rules {
		text += fmt.Sprintf("\n\n%d. %s → %s", r.ID, events[r.Event], actions[r.Action])
		if extra := automationRuleDetails(r); extra != "" {
			text += "\n" + extra
		}
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Удалить "+strconv.FormatInt(r.ID, 10), "admeventdel:"+scope+","+strconv.FormatInt(r.ID, 10))))
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Назад", "admevents:show")))
	kb := tgbotapi.NewInlineKeyboardMarkup(rows...)
	b.edit(q, text, &kb)
}
func (b *Bot) automationEventMenu(q *tgbotapi.CallbackQuery, scope string) {
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Вход участника", "admevent:"+scope+",join"), tgbotapi.NewInlineKeyboardButtonData("Выход участника", "admevent:"+scope+",leave")),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Новое сообщение", "admevent:"+scope+",message"), tgbotapi.NewInlineKeyboardButtonData("Команда", "admevent:"+scope+",command")),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Фото", "admevent:"+scope+",photo"), tgbotapi.NewInlineKeyboardButtonData("Файл", "admevent:"+scope+",file"), tgbotapi.NewInlineKeyboardButtonData("Ссылка", "admevent:"+scope+",link")),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Срабатывание антиспама", "admevent:"+scope+",antispam")),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Порог варнов", "admevent:"+scope+",warns")),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Назад", "admeventscope:"+scope)),
	)
	b.edit(q, "<b>Выбери событие</b>", &kb)
}
func (b *Bot) automationActionMenu(q *tgbotapi.CallbackQuery, scope, event string) {
	if event == "choose" {
		b.automationEventMenu(q, scope)
		return
	}
	if !validAutomationEvent(event) {
		return
	}
	rows := [][]tgbotapi.InlineKeyboardButton{tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Отправить текст", "admaction:"+scope+","+event+",send"), tgbotapi.NewInlineKeyboardButtonData("Удалить сообщение", "admaction:"+scope+","+event+",delete")), tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Добавить варн", "admaction:"+scope+","+event+",warn"))}
	if event == "warns" {
		rows = [][]tgbotapi.InlineKeyboardButton{tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Отправить текст", "admaction:"+scope+","+event+",send")), tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Бан во всей сети", "admaction:"+scope+","+event+",ban"))}
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Назад", "admevent:"+scope+",choose")))
	kb := tgbotapi.NewInlineKeyboardMarkup(rows...)
	b.edit(q, "<b>Выбери действие</b>", &kb)
}
func (b *Bot) startAutomationAction(ctx context.Context, u domain.User, q *tgbotapi.CallbackQuery, scope, event, action string) {
	if !validAutomationEvent(event) || !validAutomationAction(event, action) {
		return
	}
	if action == "ban" && !b.isOwner(u) {
		b.edit(q, "Сетевой бан может настраивать только владелец сети.", b.adminBackKeyboard())
		return
	}
	if scope == "global" && !b.isOwner(u) {
		return
	}
	chatID := q.Message.Chat.ID
	if scope == "global" {
		chatID = 0
	}
	if event == "warns" {
		s := session{Step: stepAdminAutomationValue, AdminChatID: chatID, AutomationEvent: event, AutomationAction: action}
		b.sessions.set(u.TelegramID, s)
		prompt := "Отправь число варнов, при котором должно сработать событие. Например: 3"
		if action == "send" {
			prompt = "Отправь порог и текст одним сообщением.\n\nФормат:\n3 Текст уведомления\n\nПеременные: {name}, {username}, {id}, {group}, {warns}."
		}
		b.edit(q, prompt, b.adminBackKeyboard())
		return
	}
	if action != "send" {
		if err := b.store.AddAutomationRule(ctx, domain.AutomationRule{ChatID: chatID, Event: event, Action: action}, u.TelegramID); err != nil {
			b.edit(q, "Не удалось сохранить событие.", b.adminBackKeyboard())
			return
		}
		b.edit(q, "Событие сохранено.", b.adminBackKeyboard())
		return
	}
	b.sessions.set(u.TelegramID, session{Step: stepAdminAutomationValue, AdminChatID: chatID, AutomationEvent: event, AutomationAction: action})
	b.edit(q, "Отправь текст. Переменные: {name}, {username}, {id}, {group}.", b.adminBackKeyboard())
}
func validAutomationEvent(event string) bool {
	return map[string]bool{"join": true, "leave": true, "message": true, "command": true, "photo": true, "file": true, "link": true, "antispam": true, "warns": true}[event]
}
func validAutomationAction(event, action string) bool {
	if event == "warns" {
		return action == "send" || action == "ban"
	}
	return map[string]bool{"send": true, "delete": true, "warn": true, "ban": true}[action]
}
func automationRuleDetails(r domain.AutomationRule) string {
	if r.Event != "warns" {
		return ""
	}
	limit, text := parseWarnRuleValue(r.Value)
	if limit <= 0 {
		return ""
	}
	details := "Порог: " + strconv.Itoa(limit)
	if r.Action == "send" && text != "" {
		details += "\nТекст: " + esc(text)
	}
	return details
}
func parseWarnRuleValue(raw string) (int, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, ""
	}
	lines := strings.SplitN(raw, "\n", 2)
	head := strings.TrimSpace(lines[0])
	parts := strings.Fields(head)
	if len(parts) == 0 {
		return 0, ""
	}
	limit, err := strconv.Atoi(parts[0])
	if err != nil || limit <= 0 {
		return 0, ""
	}
	if len(lines) == 2 {
		return limit, strings.TrimSpace(lines[1])
	}
	if len(parts) > 1 {
		return limit, strings.TrimSpace(strings.TrimPrefix(head, parts[0]))
	}
	return limit, ""
}
func (b *Bot) buildAutomationValue(s session, input string) (string, error) {
	if s.AutomationEvent != "warns" {
		if s.AutomationAction == "send" && (input == "" || len([]rune(input)) > 1000) {
			return "", fmt.Errorf("текст должен быть от 1 до 1000 символов")
		}
		return input, nil
	}
	parts := strings.Fields(input)
	if len(parts) == 0 {
		return "", fmt.Errorf("отправь число варнов")
	}
	limit, err := strconv.Atoi(parts[0])
	if err != nil || limit <= 0 || limit > 100 {
		return "", fmt.Errorf("число варнов должно быть от 1 до 100")
	}
	if s.AutomationAction != "send" {
		return strconv.Itoa(limit), nil
	}
	text := strings.TrimSpace(strings.TrimPrefix(input, parts[0]))
	if text == "" || len([]rune(text)) > 1000 {
		return "", fmt.Errorf("после числа нужен текст от 1 до 1000 символов")
	}
	return strconv.Itoa(limit) + "\n" + text, nil
}
func (b *Bot) addWarnAndRun(ctx context.Context, target domain.User, m *tgbotapi.Message) error {
	if err := b.store.AddWarn(ctx, target.TelegramID); err != nil {
		return err
	}
	updated, err := b.store.UserByTelegramID(ctx, target.TelegramID)
	if err != nil {
		return err
	}
	target.Warns = updated.Warns
	if m != nil {
		b.runAutomations(ctx, m, target, "warns")
	}
	return nil
}
func (b *Bot) adminGroups(ctx context.Context, q *tgbotapi.CallbackQuery) {
	registered, err := b.store.RegisteredGroups(ctx)
	if err != nil {
		return
	}
	routes, _ := b.store.NetworkGroups(ctx)
	routeByChat := make(map[int64]domain.NetworkGroup, len(routes))
	for _, route := range routes {
		routeByChat[route.ChatID] = route
	}
	var rows [][]tgbotapi.InlineKeyboardButton
	var text strings.Builder
	text.WriteString("<b>Участники сети</b>\n")
	if len(registered) == 0 {
		text.WriteString("\nНет зарегистрированных групп.")
	}
	for _, g := range registered {
		if g.ChatType == "channel" {
			fmt.Fprintf(&text, "\n\nКанал: %s", esc(g.Name))
			rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Исключить "+shortButton(g.Name), fmt.Sprintf("admsanction:%d", g.ChatID))))
			continue
		}
		route, configured := routeByChat[g.ChatID]
		if configured {
			fmt.Fprintf(&text, "\n\n%s\nЯзык: %s, тема: %d", esc(g.Name), esc(route.Language), route.ThreadID)
		} else {
			fmt.Fprintf(&text, "\n\n%s\nТема проектов не настроена", esc(g.Name))
		}
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Исключить "+shortButton(g.Name), fmt.Sprintf("admsanction:%d", g.ChatID))))
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Назад", "admmenu:show")))
	kb := tgbotapi.NewInlineKeyboardMarkup(rows...)
	b.edit(q, text.String(), &kb)
}
func (b *Bot) showUserInfo(ctx context.Context, u domain.User, chatID int64, threadID int) {
	count, _ := b.store.UserProjectCount(ctx, u.ID)
	projects, _ := b.store.ListUserProjects(ctx, u.ID)
	var names []string
	for i, p := range projects {
		if i == 5 {
			break
		}
		names = append(names, p.Name)
	}
	role := u.Role
	if b.isOwner(u) {
		role = "owner"
	} else if _, ok := b.cfg.AdminIDs[u.TelegramID]; ok && role == "member" {
		role = "admin"
	}
	roleNames := map[string]string{"owner": "Владелец сети", "admin": "Администратор", "moderator": "Модератор", "member": "Участник"}
	if title := roleNames[role]; title != "" {
		role = title
	}
	displayName := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if displayName == "" {
		displayName = u.Username
	}
	username := ""
	if u.Username != "" {
		username = "\n@" + esc(u.Username)
	}
	tags := formatProfileTags(u.Tags)
	projectBlock := "Нет добавленных проектов"
	if len(names) > 0 {
		var lines []string
		for _, name := range names {
			lines = append(lines, "• "+esc(name))
		}
		projectBlock = strings.Join(lines, "\n")
	}
	caption := fmt.Sprintf("<b>%s</b>%s\n\n<b>Уровень</b>\n%s\n\n<b>Проекты: %d</b>\n%s\n\n<b>Активность</b>: %d\n<b>Варны</b>: %d\n<b>Теги</b>: %s\n<b>Telegram ID</b>: <code>%d</code>", esc(displayName), username, esc(role), count, projectBlock, u.ActivityCount, u.Warns, tags, u.TelegramID)
	photos, err := b.api.GetUserProfilePhotos(tgbotapi.NewUserProfilePhotos(u.TelegramID))
	if err == nil && len(photos.Photos) > 0 && len(photos.Photos[0]) > 0 {
		sizes := photos.Photos[0]
		photo := tgbotapi.NewPhoto(chatID, tgbotapi.FileID(sizes[len(sizes)-1].FileID))
		photo.Caption = caption
		photo.ParseMode = "HTML"
		photo.MessageThreadID = threadID
		if _, sendErr := b.api.Send(photo); sendErr == nil {
			return
		} else {
			b.logErr(sendErr)
		}
	}
	b.sendInThread(chatID, threadID, caption, nil)
}
func (b *Bot) showRequestedUserInfo(ctx context.Context, current domain.User, m *tgbotapi.Message) {
	target := current
	var err error
	if m.ReplyToMessage != nil && m.ReplyToMessage.From != nil {
		target, err = b.store.UserByTelegramID(ctx, m.ReplyToMessage.From.ID)
	} else if argument := strings.TrimSpace(m.CommandArguments()); argument != "" {
		if id, parseErr := strconv.ParseInt(strings.TrimPrefix(argument, "@"), 10, 64); parseErr == nil {
			target, err = b.store.UserByTelegramID(ctx, id)
		} else {
			target, err = b.store.UserByUsername(ctx, argument)
		}
	}
	if err != nil {
		b.sendInThread(m.Chat.ID, m.MessageThreadID, "Пользователь не найден в сети. Он должен сначала открыть бота или написать в подключённой группе.", nil)
		return
	}
	b.showUserInfo(ctx, target, m.Chat.ID, m.MessageThreadID)
}
func (b *Bot) resolveUserReference(ctx context.Context, m *tgbotapi.Message, reference string) (domain.User, error) {
	if m.ReplyToMessage != nil && m.ReplyToMessage.From != nil {
		return b.store.UserByTelegramID(ctx, m.ReplyToMessage.From.ID)
	}
	reference = strings.TrimSpace(reference)
	if id, err := strconv.ParseInt(strings.TrimPrefix(reference, "@"), 10, 64); err == nil {
		return b.store.UserByTelegramID(ctx, id)
	}
	return b.store.UserByUsername(ctx, reference)
}
func formatProfileTags(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return "нет"
	}
	var out []string
	for _, tag := range strings.Split(raw, ",") {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		var clean []rune
		for _, r := range tag {
			if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
				clean = append(clean, r)
			} else if unicode.IsSpace(r) || r == '-' {
				clean = append(clean, '_')
			}
		}
		if len(clean) > 0 {
			out = append(out, "#"+esc(string(clean)))
		}
	}
	if len(out) == 0 {
		return "нет"
	}
	return strings.Join(out, " ")
}
func (b *Bot) runCustomCommand(ctx context.Context, m *tgbotapi.Message) bool {
	if !m.IsCommand() {
		return false
	}
	c, err := b.store.CustomCommand(ctx, strings.ToLower(m.Command()))
	if err != nil {
		return false
	}
	name := m.From.FirstName
	if name == "" {
		name = m.From.UserName
	}
	replacer := strings.NewReplacer("{name}", esc(name), "{username}", esc("@"+m.From.UserName), "{id}", strconv.FormatInt(m.From.ID, 10))
	b.sendInThread(m.Chat.ID, m.MessageThreadID, replacer.Replace(c.Response), nil)
	return true
}
func (b *Bot) checkSpam(ctx context.Context, u domain.User, m *tgbotapi.Message) bool {
	g, err := b.store.NetworkGroupByChat(ctx, m.Chat.ID)
	if err != nil || !g.AntiSpam || b.canModerate(u) {
		return false
	}
	key := fmt.Sprintf("%d:%d", m.Chat.ID, u.TelegramID)
	now := time.Now()
	if g.SpamLimit <= 0 {
		g.SpamLimit = 6
	}
	if g.SpamWindow <= 0 {
		g.SpamWindow = 10
	}
	b.spamMu.Lock()
	recent := b.spam[key][:0]
	for _, at := range b.spam[key] {
		if now.Sub(at) < time.Duration(g.SpamWindow)*time.Second {
			recent = append(recent, at)
		}
	}
	recent = append(recent, now)
	b.spam[key] = recent
	spam := len(recent) > g.SpamLimit
	b.spamMu.Unlock()
	if !spam {
		return false
	}
	_, _ = b.api.Request(tgbotapi.NewDeleteMessage(m.Chat.ID, m.MessageID))
	if g.SpamAction == "delete_warn" {
		_ = b.addWarnAndRun(ctx, u, m)
	}
	b.runAutomations(ctx, m, u, "antispam")
	return true
}

func (b *Bot) handleAutomationEvents(ctx context.Context, m *tgbotapi.Message) bool {
	if len(m.NewChatMembers) > 0 {
		for _, member := range m.NewChatMembers {
			u, err := b.store.UpsertUser(ctx, domain.User{TelegramID: member.ID, Username: member.UserName, FirstName: member.FirstName, LastName: member.LastName, LanguageCode: member.LanguageCode})
			if err == nil {
				b.runAutomations(ctx, m, u, "join")
			}
		}
		return true
	}
	if m.LeftChatMember != nil {
		u, err := b.store.UserByTelegramID(ctx, m.LeftChatMember.ID)
		if err != nil {
			u = domain.User{TelegramID: m.LeftChatMember.ID, Username: m.LeftChatMember.UserName, FirstName: m.LeftChatMember.FirstName, LastName: m.LeftChatMember.LastName}
		}
		b.runAutomations(ctx, m, u, "leave")
		return true
	}
	if m.From == nil || m.From.IsBot {
		return false
	}
	u, err := b.store.UserByTelegramID(ctx, m.From.ID)
	if err != nil {
		return false
	}
	event := "message"
	if m.IsCommand() {
		event = "command"
	} else if len(m.Photo) > 0 {
		event = "photo"
	} else if m.Document != nil {
		event = "file"
	} else if messageHasLink(m) {
		event = "link"
	}
	return b.runAutomations(ctx, m, u, event)
}
func messageHasLink(m *tgbotapi.Message) bool {
	for _, entity := range append(m.Entities, m.CaptionEntities...) {
		if entity.Type == "url" || entity.Type == "text_link" {
			return true
		}
	}
	return false
}
func (b *Bot) runAutomations(ctx context.Context, m *tgbotapi.Message, u domain.User, event string) bool {
	global, err := b.store.AutomationRules(ctx, 0, event)
	if err != nil {
		b.logErr(err)
		return false
	}
	local, err := b.store.AutomationRules(ctx, m.Chat.ID, event)
	if err != nil {
		b.logErr(err)
		return false
	}
	rules := append(global, local...)
	stop := false
	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		if event == "warns" {
			limit, _ := parseWarnRuleValue(rule.Value)
			if limit <= 0 || u.Warns != limit {
				continue
			}
		}
		if b.canModerate(u) && rule.Action != "send" {
			continue
		}
		switch rule.Action {
		case "send":
			name := strings.TrimSpace(u.FirstName + " " + u.LastName)
			if name == "" {
				name = u.Username
			}
			username := ""
			if u.Username != "" {
				username = "@" + u.Username
			}
			textValue := rule.Value
			if event == "warns" {
				_, textValue = parseWarnRuleValue(rule.Value)
			}
			text := strings.NewReplacer("{name}", esc(name), "{username}", esc(username), "{id}", strconv.FormatInt(u.TelegramID, 10), "{group}", esc(m.Chat.Title), "{warns}", strconv.Itoa(u.Warns)).Replace(textValue)
			b.sendInThread(m.Chat.ID, m.MessageThreadID, text, nil)
		case "delete":
			_, _ = b.api.Request(tgbotapi.NewDeleteMessage(m.Chat.ID, m.MessageID))
			stop = true
		case "warn":
			_ = b.addWarnAndRun(ctx, u, m)
		case "ban":
			_, _ = b.setNetworkBlock(ctx, u.TelegramID, true)
			stop = true
		}
	}
	return stop
}

func (b *Bot) missingBotRights(chatID int64) ([]string, error) {
	member, err := b.api.GetChatMember(tgbotapi.NewGetChatMember(chatID, b.api.Self.ID))
	if err != nil {
		return nil, err
	}
	if member.Status == "creator" {
		return nil, nil
	}
	if member.Status != "administrator" {
		return []string{"administrator"}, nil
	}
	chat, err := b.api.GetChat(tgbotapi.ChatInfoConfig{ChatConfig: tgbotapi.ChatConfig{ChatID: chatID}})
	if err != nil {
		return nil, err
	}
	checks := b.requiredGroupRights(member, chat.IsForum)
	var missing []string
	for _, check := range checks {
		if !check.ok {
			missing = append(missing, check.name)
		}
	}
	return missing, nil
}

func (b *Bot) requiredGroupRights(member tgbotapi.ChatMember, isForum bool) []struct {
	name string
	ok   bool
} {
	checks := []struct {
		name string
		ok   bool
	}{{"change_info", member.CanChangeInfo}, {"delete_messages", member.CanDeleteMessages}, {"restrict_members", member.CanRestrictMembers}, {"invite_users", member.CanInviteUsers}, {"pin_messages", member.CanPinMessages}, {"promote_members", member.CanPromoteMembers}}
	if isForum {
		checks = append(checks, struct {
			name string
			ok   bool
		}{"manage_topics", member.CanManageTopics})
	}
	return checks
}

func (b *Bot) syncGroupAdmins(ctx context.Context, chatID int64) []string {
	users, err := b.store.PrivilegedUsers(ctx)
	if err != nil {
		b.logErr(err)
		return []string{err.Error()}
	}
	seen := map[int64]bool{}
	for _, u := range users {
		seen[u.TelegramID] = true
	}
	ids := []int64{b.cfg.NetworkOwnerID}
	for id := range b.cfg.AdminIDs {
		ids = append(ids, id)
	}
	for _, id := range ids {
		if id == 0 || seen[id] {
			continue
		}
		if u, e := b.store.UserByTelegramID(ctx, id); e == nil {
			if id == b.cfg.NetworkOwnerID {
				u.Role = "owner"
			} else {
				u.Role = "admin"
			}
			users = append(users, u)
			seen[id] = true
		}
	}
	var failures []string
	for _, u := range users {
		if err := b.applyRoleInChat(chatID, u); err != nil {
			failures = append(failures, fmt.Sprintf("@%s: %s", u.Username, err.Error()))
			b.logErr(err)
		}
	}
	return failures
}
func (b *Bot) syncNetworkAdmins(ctx context.Context) []string {
	chats, err := b.store.RegisteredGroups(ctx)
	if err != nil {
		return []string{err.Error()}
	}
	if len(chats) == 0 {
		return []string{"в сети нет зарегистрированных групп или каналов"}
	}
	var failures []string
	for _, chat := range chats {
		missing, checkErr := b.missingSyncRights(chat.ChatID, chat.ChatType)
		if checkErr != nil {
			failures = append(failures, fmt.Sprintf("%s: проверка прав: %s", chat.Name, checkErr))
			continue
		}
		if len(missing) > 0 {
			failures = append(failures, fmt.Sprintf("%s: у бота нет %s", chat.Name, strings.Join(missing, ", ")))
			continue
		}
		for _, failure := range b.syncGroupAdmins(ctx, chat.ChatID) {
			failures = append(failures, chat.Name+": "+failure)
		}
	}
	return failures
}
func (b *Bot) missingSyncRights(chatID int64, chatType string) ([]string, error) {
	member, err := b.api.GetChatMember(tgbotapi.NewGetChatMember(chatID, b.api.Self.ID))
	if err != nil {
		return nil, err
	}
	if member.Status == "creator" {
		return nil, nil
	}
	if member.Status != "administrator" {
		return []string{"administrator"}, nil
	}
	var missing []string
	if !member.CanPromoteMembers {
		missing = append(missing, "promote_members")
	}
	if chatType == "channel" && !member.CanPostMessages {
		missing = append(missing, "post_messages")
	}
	return missing, nil
}
func forwardedChannel(m *tgbotapi.Message) *tgbotapi.Chat {
	if m == nil || m.ForwardOrigin == nil || m.ForwardOrigin.Type != tgbotapi.MessageOriginChannel {
		return nil
	}
	return m.ForwardOrigin.Chat
}
func (b *Bot) registerChannel(ctx context.Context, u domain.User, channel *tgbotapi.Chat) error {
	if channel == nil || channel.Type != "channel" {
		return fmt.Errorf("пересланное сообщение не из канала")
	}
	missing, err := b.missingSyncRights(channel.ID, "channel")
	if err != nil {
		return fmt.Errorf("бот не видит канал или не добавлен в него: %w", err)
	}
	if len(missing) > 0 {
		return fmt.Errorf("боту не хватает прав: %s", strings.Join(missing, ", "))
	}
	name := strings.TrimSpace(channel.Title)
	if name == "" {
		name = strings.TrimPrefix(channel.UserName, "@")
	}
	return b.store.RegisterGroup(ctx, domain.RegisteredGroup{Name: name, ChatID: channel.ID, ChatUsername: channel.UserName, ChatType: "channel"}, u.TelegramID)
}
func (b *Bot) syncUserRole(ctx context.Context, u domain.User) []string {
	groups, err := b.store.RegisteredGroups(ctx)
	if err != nil {
		return []string{err.Error()}
	}
	var failures []string
	for _, group := range groups {
		if err := b.applyRoleInChat(group.ChatID, u); err != nil {
			failures = append(failures, group.Name+": "+err.Error())
			b.logErr(fmt.Errorf("role in %s: %w", group.Name, err))
		}
	}
	return failures
}
func (b *Bot) applyRoleInChat(chatID int64, u domain.User) error {
	target, err := b.api.GetChatMember(tgbotapi.NewGetChatMember(chatID, u.TelegramID))
	if err != nil {
		return err
	}
	if target.Status == "creator" {
		return nil
	}
	if target.Status == "left" || target.Status == "kicked" {
		return fmt.Errorf("пользователь не состоит в группе")
	}
	botMember, err := b.api.GetChatMember(tgbotapi.NewGetChatMember(chatID, b.api.Self.ID))
	if err != nil {
		return err
	}
	privileged := u.Role == "moderator" || u.Role == "admin" || u.Role == "owner"
	rights := tgbotapi.PromoteChatMemberConfig{ChatMemberConfig: tgbotapi.NewChatMember(chatID, u.TelegramID), CanManageChat: privileged, CanDeleteMessages: privileged && botMember.CanDeleteMessages, CanInviteUsers: privileged && botMember.CanInviteUsers, CanRestrictMembers: privileged && botMember.CanRestrictMembers, CanManageTopics: privileged && botMember.CanManageTopics}
	if u.Role == "admin" || u.Role == "owner" {
		rights.CanChangeInfo = botMember.CanChangeInfo
		rights.CanPinMessages = botMember.CanPinMessages
	}
	if u.Role == "owner" {
		rights.CanPromoteMembers = botMember.CanPromoteMembers
	}
	_, err = b.api.Request(rights)
	return err
}
func (b *Bot) setNetworkBlock(ctx context.Context, telegramID int64, blocked bool) ([]string, error) {
	if err := b.store.SetUserBlocked(ctx, telegramID, blocked); err != nil {
		return nil, err
	}
	groups, err := b.store.RegisteredGroups(ctx)
	if err != nil {
		return nil, err
	}
	var failures []string
	for _, group := range groups {
		member := tgbotapi.NewChatMember(group.ChatID, telegramID)
		var request tgbotapi.Chattable
		if blocked {
			request = tgbotapi.BanChatMemberConfig{ChatMemberConfig: member, RevokeMessages: false}
		} else {
			request = tgbotapi.UnbanChatMemberConfig{ChatMemberConfig: member, OnlyIfBanned: true}
		}
		if _, requestErr := b.api.Request(request); requestErr != nil {
			failures = append(failures, group.Name)
			b.logErr(fmt.Errorf("network block in %s: %w", group.Name, requestErr))
		}
	}
	return failures, nil
}
func (b *Bot) syncBlockedUsers(ctx context.Context, chatID int64) {
	users, err := b.store.BlockedUsers(ctx)
	if err != nil {
		return
	}
	for _, u := range users {
		if _, err = b.api.Request(tgbotapi.BanChatMemberConfig{ChatMemberConfig: tgbotapi.NewChatMember(chatID, u.TelegramID), RevokeMessages: false}); err != nil {
			b.logErr(err)
		}
	}
}
func (b *Bot) send(chatID int64, text string, kb *tgbotapi.InlineKeyboardMarkup) {
	b.sendInThread(chatID, 0, text, kb)
}
func (b *Bot) sendInThread(chatID int64, threadID int, text string, kb *tgbotapi.InlineKeyboardMarkup) {
	m := tgbotapi.NewMessage(chatID, text)
	m.MessageThreadID = threadID
	m.ParseMode = "HTML"
	m.LinkPreviewOptions = tgbotapi.LinkPreviewOptions{IsDisabled: true}
	if kb != nil {
		m.ReplyMarkup = *kb
	}
	if _, err := b.api.Send(m); err != nil {
		b.logErr(err)
	}
}
func (b *Bot) sendOwnedInThread(chatID int64, threadID int, text string, kb *tgbotapi.InlineKeyboardMarkup, ownerID int64) {
	m := tgbotapi.NewMessage(chatID, text)
	m.MessageThreadID = threadID
	m.ParseMode = "HTML"
	m.LinkPreviewOptions = tgbotapi.LinkPreviewOptions{IsDisabled: true}
	if kb != nil {
		m.ReplyMarkup = *kb
	}
	sent, err := b.api.Send(m)
	if err != nil {
		b.logErr(err)
		return
	}
	b.bindButtons(sent.Chat.ID, sent.MessageID, ownerID)
}
func callbackMessageKey(chatID int64, messageID int) string {
	return fmt.Sprintf("%d:%d", chatID, messageID)
}
func (b *Bot) bindButtons(chatID int64, messageID int, ownerID int64) {
	b.buttonMu.Lock()
	b.buttonOwners[callbackMessageKey(chatID, messageID)] = ownerID
	b.buttonMu.Unlock()
}
func (b *Bot) canUseButtons(chatID int64, messageID int, userID int64) bool {
	b.buttonMu.RLock()
	owner, exists := b.buttonOwners[callbackMessageKey(chatID, messageID)]
	b.buttonMu.RUnlock()
	return !exists || owner == userID
}
func (b *Bot) edit(q *tgbotapi.CallbackQuery, text string, kb *tgbotapi.InlineKeyboardMarkup) {
	m := tgbotapi.NewEditMessageText(q.Message.Chat.ID, q.Message.MessageID, text)
	m.ParseMode = "HTML"
	m.LinkPreviewOptions = tgbotapi.LinkPreviewOptions{IsDisabled: true}
	if kb != nil {
		m.ReplyMarkup = kb
	}
	if _, err := b.api.Send(m); err != nil {
		b.logErr(err)
	}
}
func (b *Bot) scheduleDelete(chatID int64, messageID int) {
	if chatID == 0 || messageID == 0 {
		return
	}
	key := fmt.Sprintf("%d:%d", chatID, messageID)
	seconds, err := strconv.Atoi(b.store.NetworkSetting(context.Background(), "service_delete_seconds", "30"))
	if err != nil || seconds < 0 || seconds > 3600 {
		seconds = 30
	}
	b.deleteMu.Lock()
	if timer := b.deleteTimers[key]; timer != nil {
		timer.Stop()
		delete(b.deleteTimers, key)
	}
	if seconds == 0 {
		b.deleteMu.Unlock()
		return
	}
	b.deleteTimers[key] = time.AfterFunc(time.Duration(seconds)*time.Second, func() {
		_, _ = b.api.Request(tgbotapi.NewDeleteMessage(chatID, messageID))
		b.deleteMu.Lock()
		delete(b.deleteTimers, key)
		b.deleteMu.Unlock()
		b.buttonMu.Lock()
		delete(b.buttonOwners, key)
		b.buttonMu.Unlock()
	})
	b.deleteMu.Unlock()
}
func (b *Bot) languageMenu(chatID int64) {
	groups, err := b.store.NetworkGroups(context.Background())
	if err != nil || len(groups) == 0 {
		b.send(chatID, "Группы сети пока не настроены.", nil)
		return
	}
	var rows [][]tgbotapi.InlineKeyboardButton
	for i := 0; i < len(groups); i += 2 {
		var row []tgbotapi.InlineKeyboardButton
		for j := i; j < i+2 && j < len(groups); j++ {
			language := groups[j].Language
			row = append(row, tgbotapi.NewInlineKeyboardButtonData(language, "lang:"+language))
		}
		rows = append(rows, row)
	}
	kb := tgbotapi.NewInlineKeyboardMarkup(rows...)
	b.send(chatID, "Выбери язык проекта. Он определяет группу для публикации:", &kb)
}
func (b *Bot) targetForLanguage(ctx context.Context, language string) (config.ProjectTarget, bool) {
	if b.store == nil {
		return b.cfg.TargetForLanguage(language)
	}
	g, err := b.store.NetworkGroupForLanguage(ctx, language)
	if err != nil {
		return config.ProjectTarget{}, false
	}
	return config.ProjectTarget{Language: g.Language, ChatID: g.ChatID, ChatUsername: g.ChatUsername, ThreadID: g.ThreadID}, true
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

func (b *Bot) formatCatalogProject(p domain.Project) string {
	desc := p.AuthorDescription
	if desc == "" {
		desc = p.Description
	}
	if desc == "" {
		desc = "Описание не указано."
	}
	desc = truncate(desc, 220)
	links := fmt.Sprintf("<a href=\"%s\">GitHub</a>", html.EscapeString(p.RepoURL))
	if groupURL := b.groupMessageURL(p); groupURL != "" {
		links += " · <a href=\"" + html.EscapeString(groupURL) + "\">Сообщение в группе</a>"
	}
	return fmt.Sprintf("<b>%s</b> · ⭐ %s\n%s\n%s\n%s", esc(p.Name), esc(p.Stars), esc(desc), links, topicHashtags(p.Topics))
}

func (b *Bot) groupMessageURL(p domain.Project) string {
	if p.PublishedMessageID == 0 {
		return ""
	}
	target, _ := b.targetForLanguage(context.Background(), p.Language)
	username := strings.TrimPrefix(target.ChatUsername, "@")
	if username != "" {
		return fmt.Sprintf("https://t.me/%s/%d", username, p.PublishedMessageID)
	}
	chatID := strconv.FormatInt(p.PublishedChatID, 10)
	chatID = strings.TrimPrefix(chatID, "-100")
	if chatID == "" || chatID == "0" {
		return ""
	}
	return fmt.Sprintf("https://t.me/c/%s/%d", chatID, p.PublishedMessageID)
}

func formatChannelProject(p domain.Project, groupURL string) string {
	card := formatProject(p, "")
	if groupURL == "" {
		return card
	}
	return card + "\n\n<a href=\"" + html.EscapeString(groupURL) + "\">Открыть публикацию в группе</a>"
}

func truncate(value string, limit int) string {
	r := []rune(strings.TrimSpace(value))
	if len(r) <= limit {
		return string(r)
	}
	return string(r[:limit-1]) + "…"
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
