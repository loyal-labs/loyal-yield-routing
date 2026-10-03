package engine

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

// HTTPServer owns its listener and joins serving before process dependencies close.
type HTTPServer struct {
	listener net.Listener
	server   *http.Server
}

func ListenHTTP(address string, handler http.Handler) (*HTTPServer, error) {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	return &HTTPServer{listener: listener, server: &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}}, nil
}

func (s *HTTPServer) Run(ctx context.Context) error {
	done := make(chan error, 1)
	go func() { done <- s.server.Serve(s.listener) }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := s.server.Shutdown(shutdown)
		cancel()
		if err != nil {
			_ = s.server.Close()
		}
		serveErr := <-done
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
        if failure := errors.Join(err,serveErr); failure != nil { return failure }
        return ctx.Err()
	}
}

func (s *HTTPServer) Close() error { return s.server.Close() }
