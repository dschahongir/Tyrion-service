package bot

import (
	"context"
	"fmt"
	"io"
	"net/http"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// downloadFile скачивает содержимое телеграмного файла по его FileID.
//
// Схема Telegram: сначала getFile (даёт временный путь на серверах Telegram),
// затем GET на https://api.telegram.org/file/bot<token>/<path>. Токен уходит
// только в этот прямой запрос к Telegram и никуда больше не логируется.
func (b *Bot) downloadFile(ctx context.Context, fileID string) ([]byte, error) {
	file, err := b.api.GetFile(tgbotapi.FileConfig{FileID: fileID})
	if err != nil {
		return nil, fmt.Errorf("getFile: %w", err)
	}

	url := file.Link(b.api.Token)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download file: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download file: unexpected status %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read file body: %w", err)
	}
	return data, nil
}

// largestPhoto возвращает FileID самого большого варианта присланного фото.
// Telegram шлёт несколько размеров (thumbnail...original) по возрастанию —
// нам нужен последний, самый качественный.
func largestPhoto(photos []tgbotapi.PhotoSize) (string, bool) {
	if len(photos) == 0 {
		return "", false
	}
	return photos[len(photos)-1].FileID, true
}
