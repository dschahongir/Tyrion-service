// Package pipeline — общее ядро примерки, которым пользуются оба входа в
// сервис: браузерный HTTP-хендлер и Telegram-бот. Берёт фото человека и вещи
// (+ опциональные промпты), прогоняет их через ComfyUI по шаблону workflow и
// возвращает готовую картинку. Вся специфика транспорта (multipart из Gin или
// файлы из Telegram) остаётся у вызывающей стороны — сюда приходят уже готовые
// байты и имена.
package pipeline

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"tryon-service/comfyclient"
	"tryon-service/workflow"
)

// DefaultPrompt используется, если вызывающая сторона не прислала свой промпт.
const DefaultPrompt = "Replace the clothing worn by the person in image 1 with the " +
	"garment shown in image 2. Keep the person's face, pose, body shape and " +
	"background identical."

// Input — всё, что нужно для одной примерки. Name'ы должны иметь осмысленное
// расширение (.png/.jpg/...), по нему ComfyUI понимает, что это изображение.
type Input struct {
	PersonName  string
	PersonData  []byte
	GarmentName string
	GarmentData []byte
	Prompt      string // пусто → DefaultPrompt
	Negative    string // пусто → нода Negative Prompt не трогается
}

// Run выполняет одну примерку целиком: заливает оба фото, собирает свежую копию
// графа, ставит её в очередь ComfyUI и ждёт результат. Общее время ограничено
// ctx — его дедлайн задаёт вызывающая сторона.
func Run(ctx context.Context, comfy *comfyclient.Client, tpl *workflow.Template, in Input) ([]byte, error) {
	prompt := strings.TrimSpace(in.Prompt)
	if prompt == "" {
		prompt = DefaultPrompt
	}
	negative := strings.TrimSpace(in.Negative)

	// Уникальный токен на запрос: ComfyUI пишет все входные фото в общую папку
	// input/ по имени файла, а мы грузим с overwrite=true. Без токена два
	// параллельных запроса залили бы одинаковые person.png/garment.png и
	// перетёрли входы друг другу. Токен разводит имена по запросам.
	token := requestToken()
	personName := token + "-" + in.PersonName
	garmentName := token + "-" + in.GarmentName

	// 1. Заливаем оба фото во входную папку ComfyUI.
	person, err := comfy.UploadImage(ctx, personName, in.PersonData)
	if err != nil {
		return nil, fmt.Errorf("upload person: %w", err)
	}
	garment, err := comfy.UploadImage(ctx, garmentName, in.GarmentData)
	if err != nil {
		return nil, fmt.Errorf("upload garment: %w", err)
	}

	// 2. Свежая копия графа + подстановка значений по Title.
	inst, err := tpl.Instance()
	if err != nil {
		return nil, err
	}
	if err := inst.SetImage(workflow.TitlePersonImage, person.Ref()); err != nil {
		return nil, err
	}
	if err := inst.SetImage(workflow.TitleGarmentImage, garment.Ref()); err != nil {
		return nil, err
	}
	if err := inst.SetText(workflow.TitlePrompt, prompt); err != nil {
		return nil, err
	}
	// Негатив опционален: если в графе нет ноды Negative Prompt — просто пропускаем.
	if negative != "" {
		inst.TrySetText(workflow.TitleNegative, negative)
	}
	if err := inst.RandomizeSeed(workflow.TitleSampler); err != nil {
		return nil, err
	}

	// 3. Ставим в очередь и ждём картинку.
	promptID, err := comfy.Queue(ctx, inst.Graph())
	if err != nil {
		return nil, fmt.Errorf("queue prompt: %w", err)
	}
	image, err := comfy.WaitImage(ctx, promptID)
	if err != nil {
		return nil, fmt.Errorf("wait result: %w", err)
	}
	return image, nil
}

// requestToken — короткий случайный префикс, чтобы имена входных файлов были
// уникальны на каждый запрос и параллельные примерки не перетирали друг друга
// в общей папке input/ ComfyUI. 8 байт энтропии достаточно против коллизий.
func requestToken() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand практически никогда не падает; как запасной вариант
		// используем наносекунды — коллизия крайне маловероятна и не фатальна.
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
