package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/ycychome/ai-workflow/internal/config"
	"github.com/ycychome/ai-workflow/internal/orchestration"
	"github.com/ycychome/ai-workflow/internal/store"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "可信 CI 回执签发失败：", err)
		os.Exit(1)
	}
}
func run() error {
	path := flag.String("config", "", "受信控制器配置文件")
	workflow := flag.String("workflow", "", "当前工作流编号")
	runID := flag.Int64("run", 0, "已完成的 GitHub Actions 运行编号")
	key := flag.String("private-key", "", "仅宿主可读的 Ed25519 私钥文件")
	recoverOnly := flag.Bool("recover-only", false, "只回收已失去进程锁的可信签发器资源")
	flag.Parse()
	if *path == "" || *workflow == "" || (!*recoverOnly && (*runID <= 0 || *key == "")) {
		return errors.New("必须提供 config、workflow、run 和 private-key")
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return errors.New("无法加载受信控制器配置")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.Runner.AttemptMinutes)*time.Minute+5*time.Minute)
	defer cancel()
	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return errors.New("无法连接控制器数据库")
	}
	defer db.Close()
	engine, err := orchestration.New(db, cfg)
	if err != nil {
		return errors.New("无法初始化可信执行器")
	}
	if *recoverOnly {
		return engine.RecoverTrustedCIExecutor(ctx, *workflow)
	}
	result, err := engine.ExecuteTrustedCIReceipt(ctx, orchestration.CIExecutorOptions{WorkflowID: *workflow, RunID: *runID, PrivateKeyFile: *key})
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}
