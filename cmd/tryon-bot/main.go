// Command tryon-bot — Telegram-бот поверх tryon-service.
//
// Бот работает по long polling (не нужен публичный IP/webhook) и обслуживает
// одного пользователя. Всё, кроме адреса сервиса, задаётся через переменные
// окружения — секреты не должны попадать в флаги/историю команд:
//
//	TRYON_BOT_TOKEN        токен от @BotFather (обязателен)
//	TRYON_ALLOWED_USER_ID  Telegram user id единственного разрешённого юзера (обязателен)
//	TRYON_SERVICE_URL      базовый URL tryon-service (по умолч. http://127.0.0.1:8080)
//	TRYON_TIMEOUT_MIN      потолок на одну генерацию, минут (по умолч. 4)
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"tryon-service/bot"
	"tryon-service/tryonclient"
)

func main() {
	token := os.Getenv("TRYON_BOT_TOKEN")
	if token == "" {
		log.Fatal("TRYON_BOT_TOKEN не задан (получи токен у @BotFather)")
	}

	allowedRaw := os.Getenv("TRYON_ALLOWED_USER_ID")
	if allowedRaw == "" {
		log.Fatal("TRYON_ALLOWED_USER_ID не задан (свой id узнай у @userinfobot)")
	}
	allowedUserID, err := strconv.ParseInt(allowedRaw, 10, 64)
	if err != nil {
		log.Fatalf("TRYON_ALLOWED_USER_ID не число: %v", err)
	}

	serviceURL := os.Getenv("TRYON_SERVICE_URL")
	if serviceURL == "" {
		serviceURL = "http://127.0.0.1:8080"
	}

	timeout := time.Duration(envIntDefault("TRYON_TIMEOUT_MIN", 4)) * time.Minute

	client := tryonclient.New(serviceURL, timeout)

	b, err := bot.New(token, allowedUserID, client, timeout)
	if err != nil {
		log.Fatalf("инициализация бота: %v", err)
	}

	// Останавливаемся по Ctrl-C / SIGTERM — Run завершит long polling по ctx.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("tryon-bot запущен: сервис %s, timeout %v", serviceURL, timeout)
	if err := b.Run(ctx); err != nil && err != context.Canceled {
		log.Fatalf("бот остановлен с ошибкой: %v", err)
	}
}

func envIntDefault(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
