
package bot

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"tryon-service/tryonclient"
)

type state int

const (
	stateWaitingPerson  state = iota // ждём фото человека (стартовое состояние)
	stateWaitingGarment              // фото человека получено, ждём фото вещи
)

// session — состояние диалога одного чата. Живёт только в памяти:
// перезапуск бота обнуляет незаконченные диалоги, для личного бота это ок.
type session struct {
	state  state
	person []byte // байты фото человека, скачанные при получении
}

// Bot инкапсулирует Telegram API, клиент к tryon-service и состояние диалогов.
type Bot struct {
	api           *tgbotapi.BotAPI
	client        *tryonclient.Client
	allowedUserID int64
	timeout       time.Duration
	httpClient    *http.Client

	mu       sync.Mutex
	sessions map[int64]*session
}

// New создаёт бота. timeout ограничивает всю операцию генерации и должен
// совпадать (или быть чуть больше) с таймаутом tryon-service.
func New(token string, allowedUserID int64, client *tryonclient.Client, timeout time.Duration) (*Bot, error) {
	api, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, err
	}
	return &Bot{
		api:           api,
		client:        client,
		allowedUserID: allowedUserID,
		timeout:       timeout,
		httpClient:    &http.Client{Timeout: 60 * time.Second}, // только на скачивание файлов из Telegram
		sessions:      make(map[int64]*session),
	}, nil
}

// Run запускает цикл long polling до отмены ctx.
func (b *Bot) Run(ctx context.Context) error {
	log.Printf("bot authorized as @%s", b.api.Self.UserName)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
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
			if update.Message == nil {
				continue
			}
			b.handleMessage(ctx, update.Message)
		}
	}
}

func (b *Bot) handleMessage(ctx context.Context, msg *tgbotapi.Message) {
	// Контроль доступа: обслуживаем только заданного пользователя.
	if msg.From == nil || msg.From.ID != b.allowedUserID {
		b.reply(msg.Chat.ID, "Этот бот приватный.")
		return
	}

	if msg.IsCommand() {
		b.handleCommand(msg)
		return
	}
	if len(msg.Photo) > 0 {
		b.handlePhoto(ctx, msg)
		return
	}
	// Любой другой контент — подсказываем, что делать.
	b.reply(msg.Chat.ID, "Пришли фото человека, потом фото вещи. /start — начать заново.")
}

func (b *Bot) handleCommand(msg *tgbotapi.Message) {
	switch msg.Command() {
	case "start":
		b.resetSession(msg.Chat.ID)
		b.reply(msg.Chat.ID, "Привет! Пришли фото человека — потом фото вещи, и я примерю её.")
	case "cancel":
		b.resetSession(msg.Chat.ID)
		b.reply(msg.Chat.ID, "Сбросил. Пришли фото человека, чтобы начать заново.")
	default:
		b.reply(msg.Chat.ID, "Не знаю такой команды. /start или /cancel.")
	}
}

func (b *Bot) handlePhoto(ctx context.Context, msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	fileID, ok := largestPhoto(msg.Photo)
	if !ok {
		b.reply(chatID, "Не удалось прочитать фото, пришли ещё раз.")
		return
	}

	data, err := b.downloadFile(ctx, fileID)
	if err != nil {
		log.Printf("download photo: %v", err)
		b.reply(chatID, "Не смог скачать фото из Telegram, попробуй ещё раз.")
		return
	}

	sess := b.session(chatID)

	b.mu.Lock()
	switch sess.state {
	case stateWaitingPerson:
		sess.person = data
		sess.state = stateWaitingGarment
		b.mu.Unlock()
		b.reply(chatID, "Есть фото человека. Теперь пришли фото вещи (в подписи можно указать свой промпт).")

	case stateWaitingGarment:
		person := sess.person
		// Диалог завершён — сразу сбрасываем состояние, чтобы следующее фото
		// снова считалось «фото человека», даже пока идёт генерация.
		sess.person = nil
		sess.state = stateWaitingPerson
		b.mu.Unlock()

		prompt := msg.Caption
		// Генерация долгая (минуты) — уводим в горутину, чтобы бот продолжал
		// принимать апдейты (например /cancel) и не блокировал polling.
		go b.generate(chatID, person, data, prompt)
	default:
		b.mu.Unlock()
	}
}

// generate прогоняет пару фото через tryon-service и присылает результат.
func (b *Bot) generate(chatID int64, person, garment []byte, prompt string) {
	b.reply(chatID, "Генерирую примерку, это займёт пару минут…")

	ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
	defer cancel()

	result, err := b.client.TryOn(ctx, person, garment, prompt, "")
	if err != nil {
		log.Printf("tryon: %v", err)
		b.reply(chatID, "Не получилось сгенерировать: "+err.Error())
		return
	}

	photo := tgbotapi.NewPhoto(chatID, tgbotapi.FileBytes{Name: "result.png", Bytes: result})
	photo.Caption = "Готово. /start — примерить ещё."
	if _, err := b.api.Send(photo); err != nil {
		log.Printf("send result: %v", err)
	}
}

func (b *Bot) session(chatID int64) *session {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.sessions[chatID]
	if !ok {
		s = &session{state: stateWaitingPerson}
		b.sessions[chatID] = s
	}
	return s
}

func (b *Bot) resetSession(chatID int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sessions[chatID] = &session{state: stateWaitingPerson}
}

func (b *Bot) reply(chatID int64, text string) {
	if _, err := b.api.Send(tgbotapi.NewMessage(chatID, text)); err != nil {
		log.Printf("send message: %v", err)
	}
}
