// Command muse2api 是 muse2api 的 Go 版：把 muse.ai 网页端包装成 OpenAI 兼容接口。
// 对应 Python 那边 `python -m uvicorn app:app --host 0.0.0.0 --port 18610` 这一整条启动路径：
// config.py 读环境与 .env、store.py 载入账号库、engine.py 建单例、app.py 挂 lifespan 钩子，
// 这里按同样顺序显式装配一遍。
package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"muse2api-go/internal/config"
	"muse2api-go/internal/engine"
	"muse2api-go/internal/server"
	"muse2api-go/internal/store"
)

func main() {
	cfg := config.Load()
	cfg.EnsureDirs()

	st := store.New(cfg)
	eng := engine.NewEngine(cfg, engine.NewLiveHTTP(), nil)
	app := server.NewApp(cfg, st, eng)

	srv := &http.Server{
		Addr:    net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		Handler: app,
		// 不设 Read/Write timeout：Python 那边 uvicorn 也是默认无超时，
		// SSE 一条流能挂几分钟，设了写超时会把对话流掐断。
	}

	stop := make(chan struct{})
	app.Startup(stop)

	go func() {
		log.Printf("muse2api-go 已启动，监听 http://%s:%d", cfg.Host, cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("监听失败: %v", err)
		}
	}()

	// uvicorn 收到 SIGINT/SIGTERM 走优雅退出，最后触发 @app.on_event("shutdown") → engine.stop()。
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Printf("收到退出信号，正在关闭…")
	close(stop)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("等待在途请求结束超时: %v", err)
	}
	app.Shutdown()
}
