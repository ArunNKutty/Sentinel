package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/fortaegis/plasma/internal/plasma"
)

func main() {
	var (
		listen     = flag.String("listen", os.Getenv("PLASMAD_LISTEN"), "serve the network API on this address (e.g. :7443); empty serves the Unix socket only")
		certFile   = flag.String("tls-cert", plasma.TLSCert, "TLS certificate for --listen (generated if missing)")
		keyFile    = flag.String("tls-key", plasma.TLSKey, "TLS key for --listen (generated if missing)")
		tokenFile  = flag.String("token-file", plasma.TokenFile, "bearer token clients must send (generated if missing)")
		kbuild     = flag.String("kbuild-image", os.Getenv("PLASMAD_KBUILD_IMAGE"), "container image clients should build kernel modules in (default: derived from the guest kernel)")
		printLogin = flag.Bool("print-login", false, "print the `plasma engine login` command for this engine and exit")
	)
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if *printLogin {
		if err := runPrintLogin(*listen, *certFile, *keyFile, *tokenFile); err != nil {
			fmt.Fprintln(os.Stderr, "plasmad:", err)
			os.Exit(1)
		}
		return
	}

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

	store := plasma.NewArtifactStore(plasma.ArtifactsDir)
	var launchMu sync.Mutex
	labs, err := plasma.LoadLabs(plasma.LabsFile, state, store, &launchMu, log)
	if err != nil {
		log.Error("load labs", "err", err)
		os.Exit(1)
	}

	srv := plasma.NewServer(state, labs, store, &launchMu, plasma.ServerConfig{KbuildImage: *kbuild}, log)
	ln, httpSrv, err := srv.Listen(plasma.SocketPath)
	if err != nil {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
	log.Info("plasmad listening", "socket", plasma.SocketPath)
	servers := []*http.Server{httpSrv}
	go serve(log, httpSrv, ln)

	if *listen != "" {
		addr := withPort(*listen)
		cert, err := plasma.EnsureCert(*certFile, *keyFile)
		if err != nil {
			log.Error("tls", "err", err)
			os.Exit(1)
		}
		token, err := plasma.EnsureToken(*tokenFile)
		if err != nil {
			log.Error("token", "err", err)
			os.Exit(1)
		}
		tln, tsrv, err := srv.ListenTCP(addr, cert, token)
		if err != nil {
			log.Error("listen", "addr", addr, "err", err)
			os.Exit(1)
		}
		log.Info("plasmad network API listening", "addr", addr,
			"fingerprint", plasma.Fingerprint(cert.Certificate[0]), "token_file", *tokenFile,
			"login", "sudo plasmad --print-login --listen "+*listen)
		servers = append(servers, tsrv)
		go serve(log, tsrv, tln)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Info("shutting down")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, s := range servers {
		s.Shutdown(ctx)
	}
}

func serve(log *slog.Logger, srv *http.Server, ln net.Listener) {
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server", "err", err)
		os.Exit(1)
	}
}

func withPort(addr string) string {
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr
	}
	return net.JoinHostPort(addr, plasma.DefaultPort)
}

// runPrintLogin prints, for each of this host's addresses, the one command a
// client runs to use this engine. Needs root: it reads the token.
func runPrintLogin(listen, certFile, keyFile, tokenFile string) error {
	cert, err := plasma.EnsureCert(certFile, keyFile)
	if err != nil {
		return err
	}
	token, err := plasma.EnsureToken(tokenFile)
	if err != nil {
		return err
	}
	_, port, err := net.SplitHostPort(withPort(listen))
	if err != nil {
		return err
	}
	fp := plasma.Fingerprint(cert.Certificate[0])
	fmt.Println("On the client, run one of:")
	fmt.Println()
	// The default route's address first: it is the one a client on the LAN
	// most likely reaches. The hostname only helps where DNS knows it.
	hosts := []string{}
	for _, ip := range plasma.HostAddresses() {
		hosts = append(hosts, ip.String())
	}
	if h, _ := os.Hostname(); h != "" {
		hosts = append(hosts, h)
	}
	if len(hosts) == 0 {
		hosts = []string{"<engine-host>"}
	}
	for _, h := range hosts {
		fmt.Printf("  plasma engine login https://%s --token %s --fingerprint %s\n",
			net.JoinHostPort(h, port), token, fp)
	}
	fmt.Println()
	fmt.Println("or, for CI, export:")
	fmt.Printf("  PLASMA_ENGINE=https://%s PLASMA_TOKEN=%s PLASMA_FINGERPRINT=%s\n",
		net.JoinHostPort(hosts[0], port), token, fp)
	return nil
}
