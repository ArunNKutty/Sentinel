package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/fortaegis/plasma/internal/plasma"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if err := os.MkdirAll(plasma.StateDir, 0o755); err != nil {
		log.Error("create state dir", "err", err)
		os.Exit(1)
	}

	state, err := plasma.LoadState(plasma.StateFile)
	if err != nil {
		log.Error("load state", "err", err)
		os.Exit(1)
	}

	srv := plasma.NewServer(state, log)
	ln, httpSrv, err := srv.Listen(plasma.SocketPath)
	if err != nil {
		log.Error("listen", "err", err)
		os.Exit(1)
	}

	log.Info("plasmad listening", "socket", plasma.SocketPath)

	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server", "err", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Info("shutting down")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	httpSrv.Shutdown(ctx)
}
