// Package comfyclient — тонкий HTTP-клиент к локально запущенному ComfyUI.
//
// Покрывает ровно тот минимум, который нужен tryon-service:
//   - POST /upload/image   — залить входное фото (человек/вещь)
//   - POST /prompt         — поставить граф в очередь
//   - GET  /history/{id}   — дождаться результата (поллинг)
//   - GET  /view           — забрать готовую картинку
//
// ComfyUI обрабатывает очередь строго последовательно (один инстанс =
// один запрос за раз), поэтому здесь никакой хитрой конкурентности нет —
// просто ставим prompt и ждём, пока он появится в history.
package comfyclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"time"
)

// Client — обёртка вокруг одного инстанса ComfyUI.
type Client struct {
	baseURL  string
	clientID string
	http     *http.Client
}

// New создаёт клиент. baseURL — например "http://127.0.0.1:8188".
// perRequestTimeout ограничивает отдельные HTTP-вызовы (upload/queue/view),
// но НЕ общее время генерации — им управляет context, переданный в WaitImage.
func New(baseURL, clientID string, perRequestTimeout time.Duration) *Client {
	return &Client{
		baseURL:  trimSlash(baseURL),
		clientID: clientID,
		http: &http.Client{
			Timeout: perRequestTimeout,
		},
	}
}

// UploadedImage — ответ ComfyUI на /upload/image.
type UploadedImage struct {
	Name      string `json:"name"`
	Subfolder string `json:"subfolder"`
	Type      string `json:"type"`
}

// Ref возвращает ссылку в том виде, в каком её ждёт нода LoadImage
// в inputs.image: "name" или "subfolder/name".
func (u UploadedImage) Ref() string {
	if u.Subfolder != "" {
		return u.Subfolder + "/" + u.Name
	}
	return u.Name
}

// UploadImage заливает картинку во входную папку ComfyUI.
// filename должен иметь осмысленное расширение (.png/.jpg) — ComfyUI по нему
// понимает, что это изображение.
func (c *Client) UploadImage(ctx context.Context, filename string, data []byte) (UploadedImage, error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)

	part, err := w.CreateFormFile("image", filename)
	if err != nil {
		return UploadedImage{}, fmt.Errorf("create form file: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return UploadedImage{}, fmt.Errorf("write image: %w", err)
	}
	// overwrite=true — не копим мусор из имён с суффиксами (1),(2)...
	if err := w.WriteField("overwrite", "true"); err != nil {
		return UploadedImage{}, fmt.Errorf("write field: %w", err)
	}
	if err := w.WriteField("type", "input"); err != nil {
		return UploadedImage{}, fmt.Errorf("write field: %w", err)
	}
	if err := w.Close(); err != nil {
		return UploadedImage{}, fmt.Errorf("close writer: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/upload/image", &body)
	if err != nil {
		return UploadedImage{}, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := c.http.Do(req)
	if err != nil {
		return UploadedImage{}, fmt.Errorf("upload/image: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return UploadedImage{}, statusError("upload/image", resp)
	}

	var out UploadedImage
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return UploadedImage{}, fmt.Errorf("decode upload response: %w", err)
	}
	if out.Name == "" {
		return UploadedImage{}, fmt.Errorf("upload/image: empty name in response")
	}
	return out, nil
}

// Queue ставит граф (map node_id -> node) в очередь и возвращает prompt_id.
func (c *Client) Queue(ctx context.Context, graph map[string]map[string]any) (string, error) {
	payload := map[string]any{
		"prompt":    graph,
		"client_id": c.clientID,
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal prompt: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/prompt", bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("POST /prompt: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// ComfyUI на ошибке валидации графа отдаёт 400 с полезным телом —
		// пробрасываем его целиком, это единственная зацепка при отладке workflow.
		return "", statusError("POST /prompt", resp)
	}

	var out struct {
		PromptID   string                     `json:"prompt_id"`
		NodeErrors map[string]json.RawMessage `json:"node_errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode prompt response: %w", err)
	}
	if out.PromptID == "" {
		return "", fmt.Errorf("POST /prompt: empty prompt_id (node_errors: %v)", out.NodeErrors)
	}
	return out.PromptID, nil
}

// historyEntry — то, что нам нужно из ответа /history/{id}.
type historyEntry struct {
	Outputs map[string]struct {
		Images []struct {
			Filename  string `json:"filename"`
			Subfolder string `json:"subfolder"`
			Type      string `json:"type"`
		} `json:"images"`
	} `json:"outputs"`
	Status struct {
		StatusStr string `json:"status_str"`
		Completed bool   `json:"completed"`
	} `json:"status"`
}

// WaitImage поллит /history/{promptID}, пока граф не досчитается, и забирает
// первую картинку из выходов. Общее время ожидания ограничено ctx —
// на M3/16GB одна генерация это 1.5-4 минуты, дедлайн задаёт вызывающая сторона.
func (c *Client) WaitImage(ctx context.Context, promptID string) ([]byte, error) {
	const pollInterval = 2 * time.Second

	for {
		entry, ok, err := c.history(ctx, promptID)
		if err != nil {
			return nil, err
		}
		if ok {
			if entry.Status.StatusStr == "error" {
				return nil, fmt.Errorf("comfyui execution failed for prompt %s", promptID)
			}
			for _, out := range entry.Outputs {
				for _, img := range out.Images {
					if img.Type == "temp" {
						continue // промежуточные превью не берём
					}
					return c.view(ctx, img.Filename, img.Subfolder, img.Type)
				}
			}
			// history есть, но картинки в выходах нет — граф без SaveImage?
			return nil, fmt.Errorf("prompt %s finished but produced no output image", promptID)
		}

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for prompt %s: %w", promptID, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

// history возвращает запись по prompt_id. ok=false — ещё не готово (в очереди/считается).
func (c *Client) history(ctx context.Context, promptID string) (historyEntry, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/history/"+promptID, nil)
	if err != nil {
		return historyEntry{}, false, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return historyEntry{}, false, fmt.Errorf("GET /history: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return historyEntry{}, false, statusError("GET /history", resp)
	}

	// Ответ — объект {"<prompt_id>": {...}}. Пустой объект = ещё не готово.
	var raw map[string]historyEntry
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return historyEntry{}, false, fmt.Errorf("decode history: %w", err)
	}
	entry, ok := raw[promptID]
	return entry, ok, nil
}

// view забирает байты готовой картинки.
func (c *Client) view(ctx context.Context, filename, subfolder, typ string) ([]byte, error) {
	q := url.Values{}
	q.Set("filename", filename)
	q.Set("subfolder", subfolder)
	q.Set("type", typ)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/view?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET /view: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, statusError("GET /view", resp)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read image bytes: %w", err)
	}
	return data, nil
}

// SystemStats дёргает /system_stats — удобно как health-check ComfyUI при старте.
func (c *Client) SystemStats(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/system_stats", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("GET /system_stats: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return statusError("GET /system_stats", resp)
	}
	return nil
}

func statusError(op string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	return fmt.Errorf("%s: unexpected status %d: %s", op, resp.StatusCode, bytes.TrimSpace(body))
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}
