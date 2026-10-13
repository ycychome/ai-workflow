package main

import (
	"context"
	"errors"
	"flag"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ycychome/ai-workflow/fixtures/issuedesk"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "监听地址")
	seed := flag.Bool("seed", false, "生成合成演示账号")
	migrateOnly := flag.Bool("migrate-only", false, "只执行迁移后退出")
	flag.Parse()
	dsn := os.Getenv("ISSUEDESK_DATABASE_URL")
	if dsn == "" {
		log.Fatal("请配置 ISSUEDESK_DATABASE_URL；支持 Unix socket DSN")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatal("数据库连接配置无效")
	}
	defer pool.Close()
	if err = issuedesk.Migrate(ctx, pool); err != nil {
		log.Fatal(err)
	}
	if *seed {
		if err = issuedesk.Seed(ctx, pool); err != nil {
			log.Fatal("合成数据初始化失败")
		}
	}
	if *migrateOnly {
		return
	}
	server := issuedesk.HTTPServer(*addr, issuedesk.NewApp(pool))
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	log.Printf("工单服务监听 %s", *addr)
	if err = server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
