package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"rc-delete-cloud/internal/notification"
)

func main() {
	addr := flag.String("addr", ":8080", "HTTP listen address")
	dataFile := flag.String("data", "data/notifications.json", "notification storage file")
	workerInterval := flag.Duration("worker-interval", time.Second, "worker polling interval")
	flag.Parse()

	store, err := notification.NewFileStore(*dataFile)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	worker := notification.NewWorker(store, notification.NewHTTPDelivery(client), notification.WorkerConfig{
		PollInterval: *workerInterval,
		BatchSize:    20,
		SendingLease: 2 * time.Minute,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go worker.Start(ctx)

	server := &http.Server{
		Addr:              *addr,
		Handler:           notification.NewHandler(store),
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
