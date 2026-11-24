package main

import (
	"context"
	"csv-cleanroom/internal/jobs"
	"csv-cleanroom/internal/server"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "Unable to start or stop the local server:", err)
		os.Exit(1)
	}
}
func run() error {
	const address = "127.0.0.1:8080"
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	manager := jobs.New(nil)
	defer manager.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := server.HTTPServer(manager, address, ctx)
	done := make(chan error, 1)
	go func() { done <- srv.Serve(listener) }()
	fmt.Println("CSV Cleanroom: http://" + address)
	select {
	case err = <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		manager.Close()
		err = shutdownServer(srv, 10*time.Second)
		if err != nil {
			return err
		}
		return <-doneUnlessClosed(done)
	}
}
func doneUnlessClosed(done <-chan error) <-chan error {
	out := make(chan error, 1)
	go func() {
		err := <-done
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		out <- err
	}()
	return out
}

// shutdownServer keeps a bounded forced-close fallback for uncooperative handlers.
func shutdownServer(srv *http.Server, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	err := srv.Shutdown(ctx)
	if err != nil {
		_ = srv.Close()
	}
	return err
}
