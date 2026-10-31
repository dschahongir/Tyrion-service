// Package workflow загружает экспортированный из ComfyUI граф (workflow_api.json)
// и на каждый запрос отдаёт независимую копию, в которой значения подставляются
// НЕ по номеру ноды, а по её человекочитаемому Title (_meta.title).
//
// Почему по Title, а не по номеру ноды: номера нод в ComfyUI не стабильны
// (пересборка графа в UI их меняет), а Title мы задаём руками и держим в
// синхроне с константами ниже. Это позволяет пересобирать/менять граф в UI,
// не трогая Go-код — достаточно сохранить те же Title.
//
// Сам workflow_api.json НЕ хардкодится в коде: он собирается в браузерном UI
// ComfyUI под конкретную модель и экспортируется через Save (API Format).
// См. TASK.md, фаза 3.
package workflow

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
)

// Константы Title должны буквально совпадать с тем, что задано нодам в UI.
// Меняешь здесь — меняешь и в workflow_api.json (правый клик на ноде → Title).
const (
	TitlePersonImage  = "Person Image"    // нода LoadImage с фото человека
	TitleGarmentImage = "Garment Image"   // нода LoadImage с фото вещи
	TitlePrompt       = "Prompt"          // CLIPTextEncode (позитивный промпт)
	TitleNegative     = "Negative Prompt" // CLIPTextEncode (негатив) — опционально
	TitleSampler      = "KSampler"        // KSampler — в нём рандомизируем seed
)

// Template — распарсенный и провалидированный шаблон графа.
// Хранит исходные байты, чтобы каждый Instance() отдавал независимую копию
// (unmarshal из тех же байт = глубокое клонирование без ручного копирования map).
type Template struct {
	raw []byte
}

// Load читает и валидирует workflow_api.json.
func Load(path string) (*Template, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read workflow file: %w", err)
	}

	// Проверяем, что это действительно граф ComfyUI в API-формате:
	// объект node_id -> {class_type, inputs, _meta}.
	var probe map[string]node
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("parse workflow (ожидается ComfyUI API-format JSON): %w", err)
	}
	if len(probe) == 0 {
		return nil, fmt.Errorf("workflow пустой: нод не найдено")
	}

	t := &Template{raw: raw}

	// Ранняя проверка обязательных Title — падать лучше на старте сервиса,
	// чем на первом запросе пользователя.
	inst, err := t.Instance()
	if err != nil {
		return nil, err
	}
	// Garment Image НЕ обязателен: воркфлоу примерки по тексту (без фото одежды)
	// его не содержит. Его наличие проверяется при SetImage во время запроса.
	for _, title := range []string{TitlePersonImage, TitlePrompt, TitleSampler} {
		if _, err := inst.nodeByTitle(title); err != nil {
			return nil, fmt.Errorf("в workflow нет обязательной ноды: %w", err)
		}
	}
	return t, nil
}

// node — минимальная форма ноды, нужная только для проверки при Load.
// В самом Instance граф хранится как generic-map, чтобы re-marshal ничего не терял.
type node struct {
	ClassType string         `json:"class_type"`
	Inputs    map[string]any `json:"inputs"`
	Meta      struct {
		Title string `json:"title"`
	} `json:"_meta"`
}

// Instance — независимая копия графа под один запрос.
type Instance struct {
	nodes map[string]map[string]any
}

// Instance создаёт свежую копию графа. Каждый вызов независим.
func (t *Template) Instance() (*Instance, error) {
	var nodes map[string]map[string]any
	if err := json.Unmarshal(t.raw, &nodes); err != nil {
		return nil, fmt.Errorf("clone workflow: %w", err)
	}
	return &Instance{nodes: nodes}, nil
}

// Graph отдаёт граф в форме, готовой для отправки в ComfyUI /prompt.
func (in *Instance) Graph() map[string]map[string]any {
	return in.nodes
}

// SetImage подставляет ссылку на залитый файл в inputs.image ноды LoadImage.
func (in *Instance) SetImage(title, ref string) error {
	return in.setInput(title, "image", ref)
}

// SetText подставляет текст в текстовое поле ноды-энкодера промпта.
func (in *Instance) SetText(title, text string) error {
	return in.setTextInput(title, text)
}

// TrySetText — как SetText, но если ноды с таким Title нет, молча ничего не делает.
// Нужно для опционального негативного промпта: нода Negative Prompt может
// отсутствовать в графе, и это не ошибка.
func (in *Instance) TrySetText(title, text string) {
	_ = in.setTextInput(title, text)
}

// setTextInput кладёт текст в то текстовое поле, которое реально есть у ноды.
// Разные энкодеры называют поле по-разному: CLIPTextEncode — "text", а
// мультимодальные ноды Qwen-Image-Edit (TextEncodeQwenImageEdit/Plus) — "prompt".
// Выбираем по факту наличия ключа, как RandomizeSeed делает для seed/noise_seed.
func (in *Instance) setTextInput(title, text string) error {
	n, err := in.nodeByTitle(title)
	if err != nil {
		return err
	}
	inputs, err := inputsOf(n, title)
	if err != nil {
		return err
	}
	switch {
	case hasKey(inputs, "text"):
		inputs["text"] = text
	case hasKey(inputs, "prompt"):
		inputs["prompt"] = text
	default:
		return fmt.Errorf("нода %q не имеет текстового поля text/prompt", title)
	}
	return nil
}

// RandomizeSeed выставляет случайный seed у сэмплера, чтобы одинаковые входы
// не давали побайтово одинаковый результат и ComfyUI не отдавал закешированный.
func (in *Instance) RandomizeSeed(title string) error {
	n, err := in.nodeByTitle(title)
	if err != nil {
		return err
	}
	inputs, err := inputsOf(n, title)
	if err != nil {
		return err
	}
	// Разные ноды называют поле по-разному (seed / noise_seed) — ставим то,
	// что реально присутствует, чтобы не создавать лишних ключей.
	switch {
	case hasKey(inputs, "seed"):
		inputs["seed"] = randomSeed()
	case hasKey(inputs, "noise_seed"):
		inputs["noise_seed"] = randomSeed()
	default:
		return fmt.Errorf("нода %q не имеет поля seed/noise_seed", title)
	}
	return nil
}

func (in *Instance) setInput(title, key string, value any) error {
	n, err := in.nodeByTitle(title)
	if err != nil {
		return err
	}
	inputs, err := inputsOf(n, title)
	if err != nil {
		return err
	}
	inputs[key] = value
	return nil
}

// nodeByTitle ищет ноду по _meta.title. Ошибка, если не найдена или дубль.
func (in *Instance) nodeByTitle(title string) (map[string]any, error) {
	var found map[string]any
	for _, n := range in.nodes {
		meta, ok := n["_meta"].(map[string]any)
		if !ok {
			continue
		}
		if t, _ := meta["title"].(string); t == title {
			if found != nil {
				return nil, fmt.Errorf("несколько нод с Title %q — Title должен быть уникален", title)
			}
			found = n
		}
	}
	if found == nil {
		return nil, fmt.Errorf("нода с Title %q не найдена", title)
	}
	return found, nil
}

func inputsOf(n map[string]any, title string) (map[string]any, error) {
	inputs, ok := n["inputs"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("у ноды %q нет объекта inputs", title)
	}
	return inputs, nil
}

func hasKey(m map[string]any, k string) bool {
	_, ok := m[k]
	return ok
}

func randomSeed() int64 {
	// ComfyUI ждёт целочисленный seed; держим в положительном 63-битном диапазоне.
	return rand.Int63()
}
