// Package handlers — Gin-хендлеры tryon-service.
//
// Единственная содержательная ручка — POST /tryon: принимает multipart с фото
// человека и фото вещи, прогоняет их через ComfyUI (пакет pipeline) и отдаёт
// готовую картинку примерки.
package handlers

import (
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"tryon-service/comfyclient"
	"tryon-service/pipeline"
	"tryon-service/workflow"
)

// TryOn держит зависимости хендлера.
type TryOn struct {
	Comfy    *comfyclient.Client
	Workflow *workflow.Template
	// Timeout — потолок на всю генерацию (upload + очередь + сэмплинг + скачивание).
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

	image, err := pipeline.Run(ctx, h.Comfy, h.Workflow, pipeline.Input{
		PersonName:  personName,
		PersonData:  personData,
		GarmentName: garmentName,
		GarmentData: garmentData,
		Prompt:      c.PostForm("prompt"),
		Negative:    c.PostForm("negative"),
	})
	if err != nil {
		// Истёкший дедлайн — это 504, остальное — проблема апстрима (ComfyUI) → 502.
		status := http.StatusBadGateway
		if ctx.Err() == context.DeadlineExceeded {
			status = http.StatusGatewayTimeout
		}
		c.JSON(status, gin.H{"error": err.Error()})
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
