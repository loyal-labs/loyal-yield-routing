package earnmax

import (
	"context"
	"errors"
	"sync"
	"time"
)

var ErrStillRefreshing = errors.New("Still refreshing; try again shortly.")

type StaleWhileRevalidate[T any] struct {
	Fresh, Stale, Wait, Budget time.Duration

	mu      sync.Mutex
	value   *T
	at      time.Time
	err     error
	running chan struct{}
}

func (c *StaleWhileRevalidate[T]) Get(load func(context.Context) (T, error)) (T, bool, error) {
	c.mu.Lock()
	if c.running == nil && (c.value == nil || time.Since(c.at) >= c.Fresh) {
		c.running = make(chan struct{})
		go c.refresh(load, c.running)
	}
	running, current, wait := c.running, c.value == nil || time.Since(c.at) >= c.Stale, c.Wait
	if c.value == nil {
		wait = c.Budget
	}
	c.mu.Unlock()
	if current && running != nil {
		select {
		case <-running:
		case <-time.After(wait):
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	err := c.err
	if err == nil && (c.value == nil || time.Since(c.at) >= c.Stale) {
		err = ErrStillRefreshing
	}
	if c.value == nil {
		var zero T
		return zero, false, err
	}
	return *c.value, true, err
}

func (c *StaleWhileRevalidate[T]) refresh(load func(context.Context) (T, error), done chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), c.Budget)
	defer cancel()
	value, err := load(ctx)
	c.mu.Lock()
	if err == nil {
		c.value, c.at = &value, time.Now()
	}
	c.err = err
	c.running = nil
	c.mu.Unlock()
	close(done)
}
