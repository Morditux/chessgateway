package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/Morditux/chessgateway"
)

func main() {
	configPath := flag.String("config", "config.json", "path to the JSON server configuration")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("chessgateway %s\n", gateway.Version)
		return
	}

	logger := log.New(os.Stderr, "chessgateway: ", log.LstdFlags|log.LUTC)
	config, err := gateway.LoadConfig(*configPath)
	if err != nil {
		logger.Fatalf("configuration: %v", err)
	}
	server, err := gateway.NewServer(config, logger)
	if err != nil {
		logger.Fatalf("server: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger.Printf("listening on %s", config.Listen)
	if err := server.ListenAndServe(ctx); err != nil {
		logger.Fatalf("server stopped: %v", err)
	}
}
