package notification

import "time"

const (
	baseBackoff = 10 * time.Second
	maxBackoff  = 5 * time.Minute
)

func NextBackoff(attempt int) time.Duration {
	if attempt <= 1 {
		return baseBackoff
	}
	delay := baseBackoff
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay >= maxBackoff {
			return maxBackoff
		}
	}
	return delay
}
