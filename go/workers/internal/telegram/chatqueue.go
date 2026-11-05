package telegram

import (
	"context"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"
)

type chatQueue struct {
	work     func(ctx context.Context, update *Update)
	deadline time.Duration
	backlog  int
	slots    chan struct{}
	ctx      context.Context
	stop     context.CancelFunc

	mu      sync.Mutex
	chats   map[int64][]*Update
	pending int
	closed  bool
	running sync.WaitGroup
}

func newChatQueue(workers, backlog int, deadline time.Duration, work func(context.Context, *Update)) *chatQueue {
	ctx, stop := context.WithCancel(context.Background())
	return &chatQueue{
		work:     work,
		deadline: deadline,
		backlog:  backlog,
		slots:    make(chan struct{}, workers),
		ctx:      ctx,
		stop:     stop,
		chats:    map[int64][]*Update{},
	}
}

func (q *chatQueue) enqueue(chatID int64, update *Update) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || q.pending >= q.backlog {
		return false
	}
	q.pending++
	queued, busy := q.chats[chatID]
	q.chats[chatID] = append(queued, update)
	if !busy {
		q.running.Add(1)
		go q.drainChat(chatID)
	}
	return true
}

func (q *chatQueue) drainChat(chatID int64) {
	defer q.running.Done()
	for {
		q.mu.Lock()
		queued := q.chats[chatID]
		if len(queued) == 0 {
			delete(q.chats, chatID)
			q.mu.Unlock()
			return
		}
		update := queued[0]
		q.chats[chatID] = queued[1:]
		q.mu.Unlock()

		q.run(chatID, update)

		q.mu.Lock()
		q.pending--
		q.mu.Unlock()
	}
}

func (q *chatQueue) run(chatID int64, update *Update) {
	select {
	case q.slots <- struct{}{}:
		defer func() { <-q.slots }()
	case <-q.ctx.Done():
	}
	if q.ctx.Err() != nil {
		slog.Error("direct chat update dropped at shutdown", "update_id", update.UpdateID, "chat_id", chatID)
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("direct chat update panicked", "update_id", update.UpdateID, "chat_id", chatID, "panic", recovered, "stack", string(debug.Stack()))
		}
	}()
	ctx, cancel := context.WithTimeout(q.ctx, q.deadline)
	defer cancel()
	q.work(ctx, update)
}

func (q *chatQueue) drain(ctx context.Context) {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	idle := make(chan struct{})
	go func() {
		q.running.Wait()
		close(idle)
	}()
	select {
	case <-idle:
	case <-ctx.Done():
		q.stop()
		<-idle
	}
	q.stop()
}
