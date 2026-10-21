package plasma

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/user"
	"strconv"
	"sync"
	"time"
)

type Server struct {
	state *State
	log   *slog.Logger
	// Launch allocates an ID, IP and disk before committing the record, so
	// concurrent launches have to be serialized to stay distinct.
	launchMu sync.Mutex
}

func NewServer(state *State, log *slog.Logger) *Server {
	return &Server{state: state, log: log}
}

// SocketGroup owns the control socket, so members of it can drive the daemon
// without sudo. The daemon itself still needs root for taps and VM processes.
const SocketGroup = "plasma"

// ErrAlreadyRunning means another plasmad already owns the control socket.
var ErrAlreadyRunning = errors.New("another plasmad is already running")

func (s *Server) Listen(socket string) (net.Listener, *http.Server, error) {
	// Refuse to start alongside a live daemon. Unconditionally unlinking the
	// socket would let a second plasmad steal the path from a running one, and
	// because Go unlinks a unix socket when its listener closes, stopping
	// EITHER daemon then deletes the survivor's socket and cuts off every
	// client. Two daemons also both hold state.json, so whichever writes last
	// silently discards the other's VM records.
	if conn, err := net.DialTimeout("unix", socket, time.Second); err == nil {
		conn.Close()
		return nil, nil, fmt.Errorf("%w (listening on %s)", ErrAlreadyRunning, socket)
	}

	// Nothing answered, so any socket file here is stale and safe to clear.
	_ = os.Remove(socket)
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return nil, nil, err
	}
	if err := os.Chmod(socket, 0o660); err != nil {
		ln.Close()
		return nil, nil, err
	}
	if err := chownSocketGroup(socket); err != nil {
		// Not fatal: the socket stays root-only, so the CLI needs sudo.
		s.log.Warn("socket group not applied, CLI will need sudo",
			"group", SocketGroup, "err", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/vms", s.handleLaunch)
	mux.HandleFunc("GET /v1/vms", s.handleList)
	mux.HandleFunc("DELETE /v1/vms/{id}", s.handleDelete)
	mux.HandleFunc("GET /v1/vms/{id}/capture", s.handleCapture)
	mux.HandleFunc("GET /v1/topology", s.handleTopology)
	mux.HandleFunc("PUT /v1/topology/links", s.handleSetLinks)

	return ln, &http.Server{Handler: mux}, nil
}

func chownSocketGroup(socket string) error {
	grp, err := user.LookupGroup(SocketGroup)
	if err != nil {
		return fmt.Errorf("lookup group: %w", err)
	}
	gid, err := strconv.Atoi(grp.Gid)
	if err != nil {
		return fmt.Errorf("parse gid %q: %w", grp.Gid, err)
	}
	// -1 leaves the owning user alone.
	return os.Chown(socket, -1, gid)
}

func (s *Server) handleLaunch(w http.ResponseWriter, r *http.Request) {
	var req LaunchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Group == "" {
		http.Error(w, "group is required", http.StatusBadRequest)
		return
	}

	s.launchMu.Lock()
	var rec VmRecord
	var err error
	if req.ID != "" {
		// A topology file names its nodes, so the ID comes from the caller
		// rather than the <group>-N allocator.
		rec, err = LaunchNamed(req.ID, req.Group, LaunchOpts{CPUs: req.CPUs, Memory: req.Memory, Apps: req.Apps}, s.state)
	} else {
		rec, err = Launch(req.Group, s.state)
	}
	s.launchMu.Unlock()
	if err != nil {
		s.log.Error("launch failed", "group", req.Group, "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.log.Info("vm launched", "id", rec.ID, "pid", rec.PID)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(rec)
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ListResponse{VMs: s.state.List()})
}

// handleSetLinks records the links an applied topology declared. They are not
// wired to anything yet — every VM still has one NIC on the shared bridge —
// but persisting them lets the UI draw the intended topology.
func (s *Server) handleSetLinks(w http.ResponseWriter, r *http.Request) {
	var doc LinksDocument
	if err := json.NewDecoder(r.Body).Decode(&doc); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := SaveLinks(doc); err != nil {
		s.log.Error("save links", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.log.Info("topology links recorded", "topology", doc.Topology, "links", len(doc.Links))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleTopology(w http.ResponseWriter, r *http.Request) {
	// A missing links file just means no topology has been applied.
	declared, _ := LoadLinks()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(BuildTopology(s.state, func() int64 { return time.Now().Unix() }, declared))
}

// handleCapture streams pcap for as long as the client stays connected. The
// capture is bound to the request context, so a disconnect stops tcpdump —
// otherwise every dropped client would leave a root tcpdump running.
func (s *Server) handleCapture(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	iface, err := ResolveCaptureTarget(id, s.state)
	if err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, ErrNotFound):
			status = http.StatusNotFound
		case errors.Is(err, ErrNotRunning):
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}

	snaplen, err := strconv.Atoi(cmp.Or(r.URL.Query().Get("snaplen"), "0"))
	if err != nil || snaplen < 0 {
		http.Error(w, "snaplen must be a non-negative integer", http.StatusBadRequest)
		return
	}

	stream, err := StartCapture(r.Context(), CaptureOptions{
		Interface: iface,
		Filter:    r.URL.Query().Get("filter"),
		Snaplen:   snaplen,
	})
	if err != nil {
		s.log.Error("capture failed", "id", id, "iface", iface, "err", err)
		status := http.StatusInternalServerError
		if errors.Is(err, ErrBadFilter) {
			status = http.StatusBadRequest
		}
		http.Error(w, err.Error(), status)
		return
	}
	defer stream.Close()

	s.log.Info("capture started", "id", id, "iface", iface)
	w.Header().Set("Content-Type", "application/vnd.tcpdump.pcap")
	w.Header().Set("X-Plasma-Interface", iface)

	// Flush after every write: Go buffers the response body otherwise, and a
	// live Wireshark would sit empty until enough packets accumulated.
	_, copyErr := io.Copy(flushWriter{w}, stream)
	if copyErr != nil {
		s.log.Info("capture ended", "id", id, "err", copyErr)
	}
}

type flushWriter struct{ w http.ResponseWriter }

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec, err := Delete(id, s.state)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, ErrNotFound) {
			status = http.StatusNotFound
		}
		s.log.Error("delete failed", "id", id, "err", err)
		http.Error(w, err.Error(), status)
		return
	}
	s.log.Info("vm deleted", "id", rec.ID)
	w.WriteHeader(http.StatusNoContent)
}
