// Package tgbot — Telegram-бот примерки поверх того же ядра, что и веб (пакет
// pipeline). Реализован как тонкий HTTP-клиент к Telegram Bot API без внешних
// зависимостей — в том же стиле, что comfyclient.
//
// Сценарий диалога прост: пользователь присылает фото ЧЕЛОВЕКА, затем фото
// ОДЕЖДЫ (можно с подписью — она станет промптом). После второго фото бот
// запускает генерацию и присылает результат. /reset сбрасывает ожидание.
//
// Бот работает по long-polling (getUpdates), чтобы не требовать публичного URL
// и вебхуков — удобно для запуска на тестовом сервере за NAT.
package tgbot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"tryon-service/comfyclient"
	"tryon-service/pipeline"
	"tryon-service/workflow"
)

const helpText = "Привет! Я меняю одежду на фото. Два способа:\n\n" +
	"1. Пришли фото ЧЕЛОВЕКА (лучше в полный рост).\n" +
	"2. Дальше на выбор:\n" +
	"   • пришли ФОТО ОДЕЖДЫ — надену именно её (к фото можно добавить подпись-уточнение);\n" +
	"   • или напиши ТЕКСТОМ, что сделать — например «надень синее вечернее платье» " +
	"или «полностью поменяй одежду на спортивный костюм».\n\n" +
	"Генерация занимает пару минут. /reset — начать заново."

// Bot — один Telegram-бот, завязанный на общий ComfyUI/workflow.
type Bot struct {
	token   string
	api     string // https://api.telegram.org/bot<token>
	fileAPI string // https://api.telegram.org/file/bot<token>
	http    *http.Client

	comfy      *comfyclient.Client
	tplGarment *workflow.Template // примерка по фото одежды (2 изображения)
	tplPrompt  *workflow.Template // примерка по тексту (1 изображение)
	timeout    time.Duration

	mu      sync.Mutex
	pending map[int64]*pendingPhoto // chatID -> уже присланное фото человека
}

// pendingPhoto — фото человека, которое ждёт пару (фото одежды) в этом чате.
type pendingPhoto struct {
	data []byte
	ext  string
}

// New создаёт бота. token — из BotFather. comfy/tpl/timeout переиспользуются
// те же, что у веб-сервиса (оба клиента потокобезопасны для конкурентного
// использования).
func New(token string, comfy *comfyclient.Client, tplGarment, tplPrompt *workflow.Template, timeout time.Duration) *Bot {
	return &Bot{
		token:   token,
		api:     "https://api.telegram.org/bot" + token,
		fileAPI: "https://api.telegram.org/file/bot" + token,
		// Timeout клиента должен быть заметно больше long-poll таймаута getUpdates.
		http:       &http.Client{Timeout: 50 * time.Second},
		comfy:      comfy,
		tplGarment: tplGarment,
		tplPrompt:  tplPrompt,
		timeout:    timeout,
		pending:    make(map[int64]*pendingPhoto),
	}
}

// Run крутит long-polling до отмены ctx. Каждое обновление обрабатывается в
// своей горутине, чтобы долгая генерация не блокировала приём новых сообщений;
// ComfyUI всё равно выполняет задачи по очереди.
func (b *Bot) Run(ctx context.Context) error {
	const pollTimeout = 25 // сек; серверный long-poll
	offset := 0
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		updates, err := b.getUpdates(ctx, offset, pollTimeout)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("tgbot: getUpdates: %v", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(3 * time.Second):
			}
			continue
		}
		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			go b.handleUpdate(ctx, u)
		}
	}
}

func (b *Bot) handleUpdate(parent context.Context, u update) {
	msg := u.Message
	if msg == nil {
		return
	}
	chatID := msg.Chat.ID

	// Команды.
	if t := strings.TrimSpace(msg.Text); strings.HasPrefix(t, "/") {
		switch command(t) {
		case "/start", "/help":
			b.send(parent, chatID, helpText)
		case "/reset", "/cancel":
			b.mu.Lock()
			delete(b.pending, chatID)
			b.mu.Unlock()
			b.send(parent, chatID, "Сброшено. Пришли фото человека.")
		default:
			b.send(parent, chatID, "Не знаю такую команду. /help")
		}
		return
	}

	fileID, baseName, hasImg := pickImage(msg)
	text := strings.TrimSpace(msg.Text)

	// Ни картинки, ни текста (стикер, голосовое и т.п.) — просто подсказка.
	if !hasImg && text == "" {
		b.send(parent, chatID, helpText)
		return
	}

	// Пришла картинка: либо это фото человека (первое), либо фото одежды (второе).
	if hasImg {
		dlCtx, cancel := context.WithTimeout(parent, 60*time.Second)
		data, filePath, err := b.downloadFile(dlCtx, fileID)
		cancel()
		if err != nil {
			b.send(parent, chatID, "Не смог скачать фото: "+err.Error())
			return
		}
		ext := imageExt(filePath, baseName)

		b.mu.Lock()
		person := b.pending[chatID]
		if person == nil {
			// Первое фото — человек. Ждём либо фото одежды, либо текст.
			b.pending[chatID] = &pendingPhoto{data: data, ext: ext}
			b.mu.Unlock()
			b.send(parent, chatID, "Принял фото человека. Теперь пришли ФОТО ОДЕЖДЫ "+
				"— либо напиши ТЕКСТОМ, что сделать (напр. «надень синее вечернее платье»).")
			return
		}
		// Второе — одежда. Примерка по двум фото; подпись становится уточнением.
		delete(b.pending, chatID)
		b.mu.Unlock()
		b.generate(parent, chatID, b.tplGarment, pipeline.Input{
			PersonName:  "person" + person.ext,
			PersonData:  person.data,
			GarmentName: "garment" + ext,
			GarmentData: data,
			Prompt:      strings.TrimSpace(msg.Caption),
		})
		return
	}

	// Пришёл текст (не команда): это инструкция для примерки по фото человека.
	b.mu.Lock()
	person := b.pending[chatID]
	if person == nil {
		b.mu.Unlock()
		b.send(parent, chatID, helpText) // сперва нужно фото человека
		return
	}
	delete(b.pending, chatID)
	b.mu.Unlock()
	b.generate(parent, chatID, b.tplPrompt, pipeline.Input{
		PersonName: "person" + person.ext,
		PersonData: person.data,
		Prompt:     text,
	})
}

// generate прогоняет готовый вход через ComfyUI и отправляет результат в чат.
func (b *Bot) generate(parent context.Context, chatID int64, tpl *workflow.Template, in pipeline.Input) {
	b.action(parent, chatID, "upload_photo")
	b.send(parent, chatID, "Генерирую, это может занять пару минут…")

	ctx, cancel := context.WithTimeout(parent, b.timeout)
	defer cancel()
	image, err := pipeline.Run(ctx, b.comfy, tpl, in)
	if err != nil {
		b.send(parent, chatID, "Не получилось сгенерировать: "+err.Error())
		return
	}
	if err := b.sendPhoto(parent, chatID, "tryon.png", image); err != nil {
		log.Printf("tgbot: sendPhoto: %v", err)
	}
}

// pickImage выбирает изображение из сообщения: сжатое фото (берём самый крупный
// размер — он идёт последним) либо документ с image/* mime-типом.
func pickImage(msg *message) (fileID, baseName string, ok bool) {
	if n := len(msg.Photo); n > 0 {
		best := msg.Photo[n-1]
		return best.FileID, "photo.jpg", true
	}
	if d := msg.Document; d != nil && strings.HasPrefix(d.MimeType, "image/") {
		name := d.FileName
		if name == "" {
			name = "image"
		}
		return d.FileID, name, true
	}
	return "", "", false
}

// imageExt выбирает расширение для входного файла: сперва из пути Telegram,
// затем из имени документа, иначе .jpg (сжатые фото Telegram — всегда JPEG).
func imageExt(filePath, baseName string) string {
	if e := strings.ToLower(filepath.Ext(filePath)); e != "" {
		return e
	}
	if e := strings.ToLower(filepath.Ext(baseName)); e != "" {
		return e
	}
	return ".jpg"
}

// command вычленяет имя команды: "/start@MyBot foo" -> "/start".
func command(text string) string {
	t := strings.Fields(text)[0]
	if i := strings.IndexByte(t, '@'); i >= 0 {
		t = t[:i]
	}
	return t
}

// --- Telegram Bot API (тонкие обёртки) ---

type update struct {
	UpdateID int      `json:"update_id"`
	Message  *message `json:"message"`
}

type message struct {
	Chat     chat        `json:"chat"`
	Text     string      `json:"text"`
	Caption  string      `json:"caption"`
	Photo    []photoSize `json:"photo"`
	Document *document   `json:"document"`
}

type chat struct {
	ID int64 `json:"id"`
}

type photoSize struct {
	FileID string `json:"file_id"`
}

type document struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MimeType string `json:"mime_type"`
}

func (b *Bot) getUpdates(ctx context.Context, offset, timeoutSec int) ([]update, error) {
	q := url.Values{}
	q.Set("offset", strconv.Itoa(offset))
	q.Set("timeout", strconv.Itoa(timeoutSec))
	q.Set("allowed_updates", `["message"]`)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.api+"/getUpdates?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, b.errf("getUpdates: %v", err)
	}
	defer resp.Body.Close()

	var r struct {
		Ok          bool     `json:"ok"`
		Result      []update `json:"result"`
		Description string   `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, b.errf("getUpdates decode: %v", err)
	}
	if !r.Ok {
		return nil, b.errf("getUpdates: telegram error: %s", r.Description)
	}
	return r.Result, nil
}

// downloadFile получает file_path по file_id и скачивает сами байты.
func (b *Bot) downloadFile(ctx context.Context, fileID string) ([]byte, string, error) {
	q := url.Values{}
	q.Set("file_id", fileID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.api+"/getFile?"+q.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, "", b.errf("getFile: %v", err)
	}
	var r struct {
		Ok     bool `json:"ok"`
		Result struct {
			FilePath string `json:"file_path"`
		} `json:"result"`
		Description string `json:"description"`
	}
	err = json.NewDecoder(resp.Body).Decode(&r)
	resp.Body.Close()
	if err != nil {
		return nil, "", b.errf("getFile decode: %v", err)
	}
	if !r.Ok || r.Result.FilePath == "" {
		return nil, "", b.errf("getFile: telegram error: %s", r.Description)
	}

	dreq, err := http.NewRequestWithContext(ctx, http.MethodGet, b.fileAPI+"/"+r.Result.FilePath, nil)
	if err != nil {
		return nil, "", err
	}
	dresp, err := b.http.Do(dreq)
	if err != nil {
		return nil, "", b.errf("download: %v", err)
	}
	defer dresp.Body.Close()
	if dresp.StatusCode != http.StatusOK {
		return nil, "", b.errf("download: status %d", dresp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(dresp.Body, 32<<20))
	if err != nil {
		return nil, "", b.errf("download read: %v", err)
	}
	return data, r.Result.FilePath, nil
}

// send шлёт текстовое сообщение (ошибки только логируем — диалог не критичен).
func (b *Bot) send(ctx context.Context, chatID int64, text string) {
	v := url.Values{}
	v.Set("chat_id", strconv.FormatInt(chatID, 10))
	v.Set("text", text)
	if err := b.postForm(ctx, "sendMessage", v); err != nil {
		log.Printf("tgbot: sendMessage: %v", err)
	}
}

// action показывает в чате статус ("печатает"/"отправляет фото").
func (b *Bot) action(ctx context.Context, chatID int64, action string) {
	v := url.Values{}
	v.Set("chat_id", strconv.FormatInt(chatID, 10))
	v.Set("action", action)
	_ = b.postForm(ctx, "sendChatAction", v)
}

func (b *Bot) postForm(ctx context.Context, method string, v url.Values) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.api+"/"+method, strings.NewReader(v.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return b.doOK(req, method)
}

// sendPhoto отправляет картинку результата как фото (multipart-загрузка).
func (b *Bot) sendPhoto(ctx context.Context, chatID int64, name string, data []byte) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("chat_id", strconv.FormatInt(chatID, 10)); err != nil {
		return err
	}
	part, err := w.CreateFormFile("photo", name)
	if err != nil {
		return err
	}
	if _, err := part.Write(data); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.api+"/sendPhoto", &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	return b.doOK(req, "sendPhoto")
}

// doOK выполняет запрос и проверяет поле ok в ответе Telegram.
func (b *Bot) doOK(req *http.Request, method string) error {
	resp, err := b.http.Do(req)
	if err != nil {
		return b.errf("%s: %v", method, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	var r struct {
		Ok          bool   `json:"ok"`
		Description string `json:"description"`
	}
	_ = json.Unmarshal(body, &r)
	if !r.Ok {
		return b.errf("%s: telegram error: %s", method, r.Description)
	}
	return nil
}

// errf строит ошибку, вычищая из текста токен бота — иначе он утёк бы в логи
// (в URL внутри *url.Error) и в сообщения пользователю.
func (b *Bot) errf(format string, args ...any) error {
	msg := strings.ReplaceAll(fmt.Sprintf(format, args...), b.token, "***")
	return errors.New(msg)
}
