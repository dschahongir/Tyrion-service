// Command tryon-service — HTTP-сервис примерки одежды. Отдаёт браузерный UI на
// "/" (загрузка фото человека и вещи + промпт) и API POST /tryon, который
// прогоняет фото через локальный ComfyUI по шаблону workflow_api.json и
// возвращает картинку примерки. Открой http://localhost:8080 в браузере.
//
// Флаги:
//
//	--comfy-url        базовый URL ComfyUI (по умолч. http://127.0.0.1:8188)
//	--workflow         workflow примерки по фото одежды (по умолч. workflow_api.json)
//	--workflow-prompt  workflow примерки по тексту (по умолч. workflow_prompt.json)
//	--addr             адрес прослушивания (по умолч. :8080)
//	--timeout-min   потолок на одну генерацию в минутах (env TRYON_TIMEOUT_MIN)
//	--tg-token      токен Telegram-бота (env TRYON_TG_TOKEN); пусто — бот выключен
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"tryon-service/comfyclient"
	"tryon-service/handlers"
	"tryon-service/tgbot"
	"tryon-service/workflow"
)

func main() {
	var (
		comfyURL     = flag.String("comfy-url", "http://127.0.0.1:8188", "базовый URL ComfyUI")
		workflowPath = flag.String("workflow", "workflow_api.json", "путь к workflow примерки по фото одежды")
		promptPath   = flag.String("workflow-prompt", "workflow_prompt.json", "путь к workflow примерки по тексту (без фото одежды)")
		addr         = flag.String("addr", ":8080", "адрес прослушивания HTTP API")
		timeoutMin   = flag.Int("timeout-min", envIntDefault("TRYON_TIMEOUT_MIN", 4), "потолок на одну генерацию, минут")
		tgToken      = flag.String("tg-token", os.Getenv("TRYON_TG_TOKEN"), "токен Telegram-бота (пусто — бот выключен)")
	)
	flag.Parse()

	tpl, err := workflow.Load(*workflowPath)
	if err != nil {
		log.Fatalf("загрузка workflow: %v", err)
	}
	log.Printf("workflow загружен: %s", *workflowPath)

	// Второй граф — для примерки по тексту (нужен боту). Если файла нет, текстовый
	// режим просто выключается, а примерка по фото одежды продолжает работать.
	tplPrompt, err := workflow.Load(*promptPath)
	if err != nil {
		log.Printf("предупреждение: workflow-prompt не загружен (%v) — текстовый режим выключен", err)
		tplPrompt = tpl
	} else {
		log.Printf("workflow-prompt загружен: %s", *promptPath)
	}

	timeout := time.Duration(*timeoutMin) * time.Minute
	// Per-request таймаут HTTP-клиента к ComfyUI держим равным общему потолку:
	// сам поллинг результата ограничен context'ом запроса, а вот upload/queue
	// не должны падать по слишком короткому дефолту.
	comfy := comfyclient.New(*comfyURL, "tryon-service", timeout)

	// Health-check ComfyUI при старте — предупреждаем, но не падаем: ComfyUI
	// могли ещё не поднять, а сервис переживёт (проверится на первом запросе).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := comfy.SystemStats(ctx); err != nil {
		log.Printf("предупреждение: ComfyUI на %s недоступен (%v) — запускаюсь всё равно", *comfyURL, err)
	} else {
		log.Printf("ComfyUI доступен на %s", *comfyURL)
	}
	cancel()

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())
	// Приём крупных фото — поднимаем лимит на память под multipart.
	r.MaxMultipartMemory = 32 << 20 // 32 MiB

	h := &handlers.TryOn{Comfy: comfy, Workflow: tpl, Timeout: timeout}
	h.Register(r)

	// Telegram-бот — опциональный второй вход в тот же pipeline. Запускаем в
	// фоне рядом с HTTP-сервером; без токена просто не поднимаем.
	if *tgToken != "" {
		bot := tgbot.New(*tgToken, comfy, tpl, tplPrompt, timeout)
		go func() {
			log.Printf("telegram-бот запущен (long-polling)")
			if err := bot.Run(context.Background()); err != nil {
				log.Printf("telegram-бот остановлен: %v", err)
			}
		}()
	} else {
		log.Printf("telegram-бот выключен (нет --tg-token / TRYON_TG_TOKEN)")
	}

	log.Printf("tryon-service слушает %s (timeout %v)", *addr, timeout)
	if err := r.Run(*addr); err != nil {
		log.Fatalf("http server: %v", err)
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
