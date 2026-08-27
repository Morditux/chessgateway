package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/Morditux/chessgateway/client"
)

func main() {
	var configPath string
	flag.StringVar(&configPath, "config", "gatewayclient.conf", "path to the gatewayclient JSON configuration")
	flag.Parse()

	// If default path does not exist, try client/gatewayclient.conf for convenience.
	if configPath == "gatewayclient.conf" {
		if _, err := os.Stat(configPath); os.IsNotExist(err) {
			alt := filepath.Join("client", "gatewayclient.conf")
			if _, err2 := os.Stat(alt); err2 == nil {
				configPath = alt
			}
		}
	}

	cfg, err := client.LoadConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gatewayclient: configuration: %v\n", err)
		os.Exit(1)
	}

	logger, logFile, err := setupLogger(cfg.LogFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gatewayclient: log: %v\n", err)
		os.Exit(1)
	}
	if logFile != nil {
		defer logFile.Close()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Printf("gatewayclient: connecting to %s engine %s", cfg.Host, cfg.EngineID)
	if err := client.Run(ctx, cfg, os.Stdin, os.Stdout, logger); err != nil {
		if ctx.Err() != nil {
			logger.Printf("gatewayclient: stopped: %v", err)
			return
		}
		logger.Printf("gatewayclient: %v", err)
		fmt.Fprintf(os.Stderr, "gatewayclient: %v\n", err)
		os.Exit(1)
	}
	logger.Printf("gatewayclient: exiting")
}

func setupLogger(logFile string) (*log.Logger, io.Closer, error) {
	if logFile == "" {
		// Engine stderr must not pollute UCI stdout; log to stderr.
		return log.New(os.Stderr, "gatewayclient: ", log.LstdFlags|log.LUTC), nil, nil
	}
	file, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("open log_file: %w", err)
	}
	return log.New(file, "gatewayclient: ", log.LstdFlags|log.LUTC), file, nil
}
