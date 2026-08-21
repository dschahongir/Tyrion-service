// Package tryonclient — HTTP-клиент бота к tryon-service.
//
// Тонкая обёртка над POST /tryon: собирает multipart из двух картинок и
// промпта, отдаёт байты результата. Никакой логики ComfyUI здесь нет — бот
// про ComfyUI вообще не знает, он общается только с tryon-service.
package tryonclient

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"time"
)

// Client указывает на один tryon-service.
type Client struct {
	baseURL string
	http    *http.Client
}

// New создаёт клиент. baseURL — например "http://localhost:8080".
// timeout должен покрывать всю генерацию (1.5-4 минуты на M3/16GB) плюс запас.
func New(baseURL string, timeout time.Duration) *Client {
	return &Client{
		baseURL: trimSlash(baseURL),
		http:    &http.Client{Timeout: timeout},
	}
}

// TryOn отправляет фото человека и вещи в tryon-service и возвращает PNG-результат.
// prompt/negative можно оставить пустыми — сервис подставит дефолтный промпт.
func (c *Client) TryOn(ctx context.Context, person, garment []byte, prompt, negative string) ([]byte, error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)

	if err := writeFile(w, "person", "person.jpg", person); err != nil {
		return nil, err
	}
	if err := writeFile(w, "garment", "garment.jpg", garment); err != nil {
		return nil, err
	}
	if prompt != "" {
		if err := w.WriteField("prompt", prompt); err != nil {
			return nil, fmt.Errorf("write prompt field: %w", err)
		}
	}
	if negative != "" {
		if err := w.WriteField("negative", negative); err != nil {
			return nil, fmt.Errorf("write negative field: %w", err)
		}
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("close multipart: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/tryon", &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST /tryon: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// tryon-service на ошибке отдаёт JSON {"error": "..."} — показываем как есть.
		return nil, fmt.Errorf("tryon-service вернул %d: %s", resp.StatusCode, bytes.TrimSpace(data))
	}
	return data, nil
}

func writeFile(w *multipart.Writer, field, filename string, data []byte) error {
	part, err := w.CreateFormFile(field, filename)
	if err != nil {
		return fmt.Errorf("create form file %q: %w", field, err)
	}
	if _, err := part.Write(data); err != nil {
		return fmt.Errorf("write %q: %w", field, err)
	}
	return nil
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}
