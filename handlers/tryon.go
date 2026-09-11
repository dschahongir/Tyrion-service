// Package handlers — Gin-хендлеры tryon-service.
//
// Единственная содержательная ручка — POST /tryon: принимает multipart с фото
// человека и фото вещи, прогоняет их через ComfyUI по шаблону workflow и
// отдаёт готовую картинку примерки.
package handlers

import (
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"tryon-service/comfyclient"
	"tryon-service/workflow"
)

// DefaultPrompt используется, если клиент не прислал свой промпт.
const DefaultPrompt = "Replace the clothing worn by the person in image 1 with the " +
	"garment shown in image 2. Keep the person's face, pose, body shape and " +
	"background identical."

// TryOn держит зависимости хендлера.
type TryOn struct {
	Comfy    *comfyclient.Client
	Workflow *workflow.Template
	// Timeout — потолок на всю генерацию (upload + очередь + сэмплинг + скачивание).
	// На M3/16GB реально 1.5-4 минуты, поэтому дефолт крупный (см. --timeout-min).
	Timeout time.Duration
}

// Register навешивает маршруты на роутер.
func (h *TryOn) Register(r gin.IRoutes) {
	r.GET("/", h.index)
	r.GET("/healthz", h.health)
	r.POST("/tryon", h.handle)
}

func (h *TryOn) health(c *gin.Context) {
	// Health-check самого сервиса; заодно пингуем ComfyUI, чтобы 200 отражал
	// реальную готовность цепочки, а не только то, что Go-процесс жив.
	ctx, cancel := contextWithTimeout(c, 5*time.Second)
	defer cancel()
	if err := h.Comfy.SystemStats(ctx); err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "comfyui unreachable", "error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (h *TryOn) handle(c *gin.Context) {
	ctx, cancel := contextWithTimeout(c, h.Timeout)
	defer cancel()

	personName, personData, err := readImageField(c, "person")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	garmentName, garmentData, err := readImageField(c, "garment")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	prompt := strings.TrimSpace(c.PostForm("prompt"))
	if prompt == "" {
		prompt = DefaultPrompt
	}
	negative := strings.TrimSpace(c.PostForm("negative"))

	// 1. Заливаем оба фото во входную папку ComfyUI.
	person, err := h.Comfy.UploadImage(ctx, personName, personData)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("upload person: %v", err)})
		return
	}
	garment, err := h.Comfy.UploadImage(ctx, garmentName, garmentData)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("upload garment: %v", err)})
		return
	}

	// 2. Свежая копия графа + подстановка значений по Title.
	inst, err := h.Workflow.Instance()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := inst.SetImage(workflow.TitlePersonImage, person.Ref()); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := inst.SetImage(workflow.TitleGarmentImage, garment.Ref()); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := inst.SetText(workflow.TitlePrompt, prompt); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// Негатив опционален: если в графе нет ноды Negative Prompt — просто пропускаем.
	if negative != "" {
		inst.TrySetText(workflow.TitleNegative, negative)
	}
	if err := inst.RandomizeSeed(workflow.TitleSampler); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// 3. Ставим в очередь и ждём картинку.
	promptID, err := h.Comfy.Queue(ctx, inst.Graph())
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("queue prompt: %v", err)})
		return
	}
	image, err := h.Comfy.WaitImage(ctx, promptID)
	if err != nil {
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": fmt.Sprintf("wait result: %v", err)})
		return
	}

	c.Data(http.StatusOK, "image/png", image)
}

// readImageField достаёт файл из multipart-поля и возвращает безопасное имя
// (с расширением) и его байты.
func readImageField(c *gin.Context, field string) (string, []byte, error) {
	fh, err := c.FormFile(field)
	if err != nil {
		return "", nil, fmt.Errorf("missing form file %q", field)
	}
	data, err := readMultipart(fh)
	if err != nil {
		return "", nil, fmt.Errorf("read %q: %w", field, err)
	}
	return safeImageName(field, fh.Filename), data, nil
}

func readMultipart(fh *multipart.FileHeader) ([]byte, error) {
	f, err := fh.Open()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// safeImageName формирует имя вида "person.png" — фиксированный базовый префикс
// плюс расширение исходника (или .png по умолчанию). Своё имя пользователя не
// используем, чтобы не тащить в ComfyUI пути/спецсимволы.
func safeImageName(field, original string) string {
	ext := strings.ToLower(filepath.Ext(original))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".webp":
	default:
		ext = ".png"
	}
	return field + ext
}
