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

	for _, dir := range []string{plasma.StateDir, plasma.RunDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Error("create dir", "dir", dir, "err", err)
			os.Exit(1)
		}
	}

	if moved, err := plasma.AdoptLegacyState(plasma.LegacyStateFile, plasma.StateFile); err != nil {
		log.Error("adopt legacy state", "err", err)
		os.Exit(1)
	} else if moved {
		log.Info("adopted state from legacy path", "from", plasma.LegacyStateFile, "to", plasma.StateFile)
	}

	state, err := plasma.LoadState(plasma.StateFile)
	if err != nil {
		log.Error("load state", "err", err)
		os.Exit(1)
	}

	// State survives reboots; the VMs and their /var/run scratch do not.
	if stale, err := state.Reconcile(); err != nil {
		log.Error("reconcile state", "err", err)
	} else if len(stale) > 0 {
		log.Warn("vms no longer running, marked exited", "ids", stale)
	}
	if orphans, err := plasma.OrphanDisks(state); err != nil {
		log.Warn("scan runtime disks", "err", err)
	} else if len(orphans) > 0 {
		log.Warn("unclaimed disks in runtime dir, remove them if unwanted", "disks", orphans)
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
