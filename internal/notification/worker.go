package notification

import (
	"context"
	"log"
	"sync"
	"time"
)

type WorkerConfig struct {
	PollInterval time.Duration
	BatchSize    int
	SendingLease time.Duration
}

type Worker struct {
	store    Store
	delivery Delivery
	config   WorkerConfig
}

func NewWorker(store Store, delivery Delivery, config WorkerConfig) Worker {
	if store == nil {
		panic("store is required")
	}
	if delivery == nil {
		panic("delivery is required")
	}
	if config.PollInterval <= 0 {
		config.PollInterval = time.Second
	}
	if config.BatchSize <= 0 {
		config.BatchSize = 10
	}
	if config.SendingLease <= 0 {
		config.SendingLease = 2 * time.Minute
	}
	return Worker{store: store, delivery: delivery, config: config}
}

func (w Worker) Start(ctx context.Context) {
	ticker := time.NewTicker(w.config.PollInterval)
	defer ticker.Stop()

	for {
		w.runOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w Worker) RunOnce(ctx context.Context) {
	w.runOnce(ctx)
}

func (w Worker) runOnce(ctx context.Context) {
	due, err := w.store.FetchDue(time.Now(), w.config.BatchSize, w.config.SendingLease)
	if err != nil {
		log.Printf("fetch due notifications: %v", err)
		return
	}
	var wg sync.WaitGroup
	for _, n := range due {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			w.process(ctx, id)
		}(n.ID)
	}
	wg.Wait()
}

func (w Worker) process(ctx context.Context, id string) {
	now := time.Now()
	n, err := w.store.MarkSending(id, now)
	if err != nil {
		log.Printf("mark notification sending: %v", err)
		return
	}
	result := w.delivery.Send(ctx, n)
	now = time.Now()
	if result.Success {
		if err := w.store.MarkSuccess(n.ID, now); err != nil {
			log.Printf("mark notification success: %v", err)
		}
		return
	}

	message := result.ErrorMessage
	if message == "" {
		message = "delivery failed"
	}
	if !result.Retryable || n.AttemptCount >= n.MaxAttempts {
		if err := w.store.MarkFailed(n.ID, message, now); err != nil {
			log.Printf("mark notification failed: %v", err)
		}
		return
	}

	nextRetryAt := now.Add(NextBackoff(n.AttemptCount))
	if result.RetryAfter != nil && result.RetryAfter.After(nextRetryAt) {
		nextRetryAt = *result.RetryAfter
	}
	if err := w.store.MarkRetry(n.ID, message, nextRetryAt, now); err != nil {
		log.Printf("mark notification retrying: %v", err)
	}
}
