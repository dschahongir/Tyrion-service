package handlers

import (
	_ "embed"
	"net/http"

	"github.com/gin-gonic/gin"
)

// indexHTML — встроенная в бинарь браузерная страница примерки. Лежит рядом
// с кодом (handlers/index.html) и вшивается через go:embed, чтобы сервис был
// одним самодостаточным бинарём без внешней папки со статикой.
//
//go:embed index.html
var indexHTML []byte

// index отдаёт браузерный UI: загрузка фото человека и вещи, промпт и показ
// результата. Вся логика — прямой POST /tryon из браузера, отдельного API
// для страницы не нужно.
func (h *TryOn) index(c *gin.Context) {
	c.Data(http.StatusOK, "text/html; charset=utf-8", indexHTML)
}
