// mnemo-bot — mnemosync 第一方 bot 运行时.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/HarryHello/mnemo-bot/internal/app"
	"github.com/HarryHello/mnemo-bot/internal/config"
)

const version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		serve(os.Args[2:])
	case "check":
		checkCmd(os.Args[2:])
	case "version":
		fmt.Println("mnemo-bot", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "未知命令: %s\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `mnemo-bot — mnemosync 第一方 bot 运行时 (设计文档: docs/design.md)

用法:
  mnemo-bot serve  [-config <path>]   运行 bot 运行时 (反向 WS + 触发管线)
  mnemo-bot check  [-config <path>]   自检 mnemosync 版本/鉴权/events 端点
  mnemo-bot version                  打印版本
`)
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgFlag := fs.String("config", "", "配置文件路径 (默认自动查找 config.local.toml / config.toml)")
	_ = fs.Parse(args)

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg, err := config.Load(config.ResolvePath(*cfgFlag))
	if err != nil {
		log.Error("配置加载失败", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := app.Run(ctx, cfg, log); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("运行失败", "err", err)
		os.Exit(1)
	}
	log.Info("mnemo-bot 已退出")
}

func checkCmd(args []string) {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	cfgFlag := fs.String("config", "", "配置文件路径 (默认自动查找 config.local.toml / config.toml)")
	_ = fs.Parse(args)

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	os.Exit(app.Check(config.ResolvePath(*cfgFlag), log))
}
