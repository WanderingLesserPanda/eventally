// Command eventally runs the Eventally web server.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/WanderingLesserPanda/eventally/internal/config"
	"github.com/WanderingLesserPanda/eventally/internal/db"
	"github.com/WanderingLesserPanda/eventally/internal/web"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	database, err := db.Open(ctx, cfg.DSN)
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	defer database.Close()

	app, err := web.New(database.Pool, "internal/web/templates")
	if err != nil {
		log.Fatalf("templates: %v", err)
	}

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           app.Router(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("eventally listening on %s", cfg.Addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server: %v", err)
	}
	log.Printf("shut down cleanly")
}
