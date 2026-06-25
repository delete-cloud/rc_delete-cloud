package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"rc_delete-cloud/internal/notification"
)

func main() {
	addr := flag.String("addr", ":8080", "HTTP listen address")
	dbPath := flag.String("db", "data/notifications.db", "SQLite database file")
	allowedHosts := flag.String("allowed-hosts", "", "comma-separated target host allowlist; empty allows any public host")
	workerInterval := flag.Duration("worker-interval", time.Second, "worker polling interval")
	flag.Parse()

	store, err := notification.NewSQLiteStore(*dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	policy := notification.NewSecurityPolicy(splitCSV(*allowedHosts), notification.DefaultAllowedHeaders())
	client := &http.Client{Timeout: 5 * time.Second}
	worker := notification.NewWorker(store, notification.NewHTTPDeliveryWithSecurity(client, policy), notification.WorkerConfig{
		PollInterval: *workerInterval,
		BatchSize:    20,
		SendingLease: 2 * time.Minute,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go worker.Start(ctx)

	server := &http.Server{
		Addr:              *addr,
		Handler:           notification.NewHandlerWithSecurity(store, policy),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown server: %v", err)
		}
	}()

	log.Printf("notification service listening on %s", *addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("listen: %v", err)
	}
}

func splitCSV(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	items := make([]string, 0, len(parts))
	for _, part := range parts {
		item := strings.TrimSpace(part)
		if item == "" {
			continue
		}
		items = append(items, item)
	}
	return items
}
