package telegram

import (
	"context"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"
)

type directReply struct {
	update    *Update
	threadID  string
	messageID string
}

type chatQueue struct {
	work     func(ctx context.Context, reply directReply)
	deadline time.Duration
	backlog  int
	slots    chan struct{}
	ctx      context.Context
	stop     context.CancelFunc

	mu       sync.Mutex
	chats    map[int64][]directReply
	reserved int
	closed   bool
	running  sync.WaitGroup
}

func newChatQueue(workers, backlog int, deadline time.Duration, work func(context.Context, directReply)) *chatQueue {
	ctx, stop := context.WithCancel(context.Background())
	return &chatQueue{
		work:     work,
		deadline: deadline,
		backlog:  backlog,
		slots:    make(chan struct{}, workers),
		ctx:      ctx,
		stop:     stop,
		chats:    map[int64][]directReply{},
	}
}

func (q *chatQueue) reserve() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || q.reserved >= q.backlog {
		return false
	}
	q.reserved++
	q.running.Add(1)
	return true
}

func (q *chatQueue) release() {
	q.mu.Lock()
	q.reserved--
	q.mu.Unlock()
	q.running.Done()
}

func (q *chatQueue) submit(chatID int64, reply directReply) {
	q.mu.Lock()
	defer q.mu.Unlock()
	queued, busy := q.chats[chatID]
	q.chats[chatID] = append(queued, reply)
	if !busy {
		go q.drainChat(chatID)
	}
}

func (q *chatQueue) drainChat(chatID int64) {
	for {
		q.mu.Lock()
		queued := q.chats[chatID]
		if len(queued) == 0 {
			delete(q.chats, chatID)
			q.mu.Unlock()
			return
		}
		reply := queued[0]
		q.chats[chatID] = queued[1:]
		q.mu.Unlock()

		q.run(chatID, reply)
		q.release()
	}
}

func (q *chatQueue) run(chatID int64, reply directReply) {
	select {
	case q.slots <- struct{}{}:
		defer func() { <-q.slots }()
	case <-q.ctx.Done():
	}
	if q.ctx.Err() != nil {
		slog.Error("direct chat reply dropped at shutdown", "update_id", reply.update.UpdateID, "chat_id", chatID)
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("direct chat update panicked", "update_id", reply.update.UpdateID, "chat_id", chatID, "panic", recovered, "stack", string(debug.Stack()))
		}
	}()
	ctx, cancel := context.WithTimeout(q.ctx, q.deadline)
	defer cancel()
	q.work(ctx, reply)
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
