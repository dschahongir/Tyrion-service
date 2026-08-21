package handlers

import (
	"context"
	"time"

	"github.com/gin-gonic/gin"
)

// contextWithTimeout — контекст запроса, ограниченный сверху заданным таймаутом.
// Наследуемся от c.Request.Context(), чтобы обрыв соединения клиентом тоже
// отменял работу (ComfyUI-поллинг завершится).
func contextWithTimeout(c *gin.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(c.Request.Context(), timeout)
}
