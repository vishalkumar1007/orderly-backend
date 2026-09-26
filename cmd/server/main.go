package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	// Opening hours are evaluated in the tenant's own timezone. Embedding the
	// zone database means a container with no system tzdata still resolves
	// "Asia/Kolkata" correctly instead of silently falling back to UTC.
	_ "time/tzdata"

	"github.com/joho/godotenv"

	"github.com/orderly/orderly-backend/internal/config"
	"github.com/orderly/orderly-backend/internal/httpserver"
	"github.com/orderly/orderly-backend/pkg/database"
	"github.com/orderly/orderly-backend/pkg/logger"
)

func main() {
	_ = godotenv.Load()

	cfg, err := config.Load()
	if err != nil {
		panic(err)
	}

	log := logger.New(cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := database.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("failed to connect to database", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	api := httpserver.New(log, pool, cfg)

	// Move any pre-existing plaintext SMTP password into the encrypted store
	// before serving traffic, so no request can observe the legacy value.
	if err := api.MigrateLegacyConfig(ctx); err != nil {
		log.Error("legacy configuration migration failed", "error", err)
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           api.Router(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Info("server listening", "addr", cfg.HTTPAddr, "env", cfg.AppEnv)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}
