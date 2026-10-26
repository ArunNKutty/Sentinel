package plasma

import (
	"cmp"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/user"
	"runtime"
	"strconv"
	"sync"
	"time"
)

type Server struct {
	state  *State
	labs   *Labs
	store  *ArtifactStore
	log    *slog.Logger
	config ServerConfig
	// Launch allocates an ID, IP and disk before committing the record, so
	// concurrent launches have to be serialized to stay distinct. Shared
	// with labs, which launch through the same allocator.
	launchMu *sync.Mutex
}

// ServerConfig is what the engine advertises beyond what it can read itself.
type ServerConfig struct {
	// KbuildImage overrides the build image derived from the guest kernel.
	KbuildImage string
}

func NewServer(state *State, labs *Labs, store *ArtifactStore, launchMu *sync.Mutex, cfg ServerConfig, log *slog.Logger) *Server {
	return &Server{state: state, labs: labs, store: store, launchMu: launchMu, config: cfg, log: log}
}

// SocketGroup owns the control socket, so members of it can drive the daemon
// without sudo. The daemon itself still needs root for taps and VM processes.
const SocketGroup = "plasma"

// ErrAlreadyRunning means another plasmad already owns the control socket.
var ErrAlreadyRunning = errors.New("another plasmad is already running")

// maxRequestBody caps JSON requests; artifacts have their own, larger cap.
const maxRequestBody = 64 << 20

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	// The legacy, unscoped fleet.
	mux.HandleFunc("POST /v1/vms", s.handleLaunch)
	mux.HandleFunc("GET /v1/vms", s.handleList)
	mux.HandleFunc("DELETE /v1/vms/{id}", s.handleDelete)
	mux.HandleFunc("GET /v1/vms/{id}/capture", s.handleCapture)
	mux.HandleFunc("GET /v1/vms/{id}/console", s.handleConsole)
	mux.HandleFunc("GET /v1/topology", s.handleTopology)
	mux.HandleFunc("PUT /v1/topology/links", s.handleSetLinks)

	mux.HandleFunc("GET /v1/engine", s.handleEngine)
	mux.HandleFunc("HEAD /v1/artifacts/{digest}", s.handleArtifactHead)
	mux.HandleFunc("PUT /v1/artifacts/{digest}", s.handleArtifactPut)

	mux.HandleFunc("POST /v1/labs", s.handleCreateLab)
	mux.HandleFunc("GET /v1/labs", s.handleListLabs)
	mux.HandleFunc("GET /v1/labs/{lab}", s.handleLabStatus)
	mux.HandleFunc("DELETE /v1/labs/{lab}", s.handleDeleteLab)
	mux.HandleFunc("GET /v1/labs/{lab}/topology", s.handleLabTopology)
	mux.HandleFunc("GET /v1/labs/{lab}/capture", s.handleLabCapture)
	mux.HandleFunc("GET /v1/labs/{lab}/vms/{vm}/console", s.handleLabConsole)
	mux.HandleFunc("GET /v1/labs/{lab}/vms/{vm}/tunnel/{port}", s.handleTunnel)
	for _, m := range []string{"PUT", "POST"} {
		mux.HandleFunc(m+" /v1/labs/{lab}/links/{link}/mirror", s.handleSetMirror)
		mux.HandleFunc(m+" /v1/labs/{lab}/links/{link}/filters", s.handleSetFilter)
		mux.HandleFunc(m+" /v1/labs/{lab}/links/{link}/drop", s.handleSetDrop)
	}
	mux.HandleFunc("GET /v1/labs/{lab}/links/{link}/mirror", s.handleMirrorStatus)
	mux.HandleFunc("GET /v1/labs/{lab}/links/{link}/drop", s.handleDropStatus)
	mux.HandleFunc("DELETE /v1/labs/{lab}/links/{link}/drop", s.handleClearDrop)
	mux.HandleFunc("DELETE /v1/labs/{lab}/links/{link}/mirror", s.handleClearMirror)
	mux.HandleFunc("DELETE /v1/labs/{lab}/links/{link}/filters", s.handleClearFilter)
	return mux
}

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
	return ln, &http.Server{Handler: s.handler(), ReadHeaderTimeout: 10 * time.Second}, nil
}

// ListenTCP is the network API: TLS, and every call carries the engine's
// token. Same handler as the socket, so a remote client can do exactly what
// a local one can.
func (s *Server) ListenTCP(addr string, cert tls.Certificate, token string) (net.Listener, *http.Server, error) {
	ln, err := tls.Listen("tcp", addr, ServerTLSConfig(cert))
	if err != nil {
		return nil, nil, err
	}
	srv := &http.Server{
		Handler:           requireToken(token, s.handler()),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// An empty map turns HTTP/2 off: streams hijack the connection.
		TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){},
		ErrorLog:     slog.NewLogLogger(s.log.Handler(), slog.LevelDebug),
	}
	return ln, srv, nil
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

// httpStatus maps the package's errors to what a client should see.
func httpStatus(err error) int {
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrLabNotFound), errors.Is(err, ErrNoArtifact), errors.Is(err, ErrNotSet):
		return http.StatusNotFound
	case errors.Is(err, ErrNotRunning), errors.Is(err, ErrLabExists), errors.Is(err, ErrNoCapacity):
		return http.StatusConflict
	case errors.Is(err, ErrBadLab), errors.Is(err, ErrBadFilter):
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

func writeErr(w http.ResponseWriter, err error) {
	http.Error(w, err.Error(), httpStatus(err))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody)).Decode(v); err != nil {
		http.Error(w, "bad request body: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func (s *Server) handleLaunch(w http.ResponseWriter, r *http.Request) {
	var req LaunchRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Group == "" {
		http.Error(w, "group is required", http.StatusBadRequest)
		return
	}
	// App paths here are files on the engine, read by a root daemon. Over
	// the network that would hand any token holder every file on the host,
	// so remote apps go through labs, which only read uploaded artifacts.
	if len(req.Apps) > 0 && fromNetwork(r) {
		http.Error(w, "apps by host path are local-only; use a lab (POST /v1/labs, plasma lab apply)", http.StatusForbidden)
		return
	}

	s.launchMu.Lock()
	var rec VmRecord
	var err error
	if req.ID != "" {
		// A topology file names its nodes, so the ID comes from the caller
		// rather than the <group>-N allocator.
		rec, err = LaunchNamed(req.ID, req.Group, LaunchOpts{CPUs: req.CPUs, Memory: req.Memory, Apps: req.Apps, Nics: req.Nics}, s.state)
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
	writeJSON(w, http.StatusOK, rec)
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, ListResponse{VMs: s.state.List()})
}

// handleSetLinks records the links an applied topology declared. The wiring
// itself arrived with each VM's launch (LaunchRequest.Nics); the declared form
// lets /v1/topology still report a link whose VM is not running.
func (s *Server) handleSetLinks(w http.ResponseWriter, r *http.Request) {
	var doc LinksDocument
	if !decodeJSON(w, r, &doc) {
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
	writeJSON(w, http.StatusOK, BuildTopology(s.state, func() int64 { return time.Now().Unix() }, declared))
}

func (s *Server) handleCapture(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	iface, err := ResolveCaptureTarget(id, s.state)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.streamCapture(w, r, iface, id)
}

func (s *Server) handleLabCapture(w http.ResponseWriter, r *http.Request) {
	lab, target := r.PathValue("lab"), r.URL.Query().Get("target")
	if target == "" {
		http.Error(w, "target is required (mgmt, <vm>, <vm>:<ethN> or a bridge node)", http.StatusBadRequest)
		return
	}
	iface, err := s.labs.CaptureTarget(lab, target)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.streamCapture(w, r, iface, lab+"/"+target)
}

// streamCapture streams pcap for as long as the client stays connected. The
// capture is bound to the request context, so a disconnect stops tcpdump —
// otherwise every dropped client would leave a root tcpdump running.
func (s *Server) streamCapture(w http.ResponseWriter, r *http.Request, iface, label string) {
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
		s.log.Error("capture failed", "target", label, "iface", iface, "err", err)
		writeErr(w, err)
		return
	}
	defer stream.Close()

	s.log.Info("capture started", "target", label, "iface", iface, "remote", r.RemoteAddr)
	w.Header().Set("Content-Type", "application/vnd.tcpdump.pcap")
	w.Header().Set("X-Plasma-Interface", iface)

	// Flush after every write: Go buffers the response body otherwise, and a
	// live Wireshark would sit empty until enough packets accumulated.
	_, copyErr := io.Copy(flushWriter{w}, stream)
	if copyErr != nil {
		s.log.Info("capture ended", "target", label, "err", copyErr)
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
	rec, err := Delete(LabNet{}, id, s.state)
	if err != nil {
		s.log.Error("delete failed", "id", id, "err", err)
		writeErr(w, err)
		return
	}
	s.log.Info("vm deleted", "id", rec.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleConsole(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.state.Get("", r.PathValue("id"))
	if !ok {
		writeErr(w, fmt.Errorf("%w: %s", ErrNotFound, r.PathValue("id")))
		return
	}
	if rec.Status != StatusRunning {
		writeErr(w, fmt.Errorf("%w: %s is %s", ErrNotRunning, rec.ID, rec.Status))
		return
	}
	s.serveConsole(w, r, rec)
}

func (s *Server) handleLabConsole(w http.ResponseWriter, r *http.Request) {
	_, rec, err := s.labs.VM(r.PathValue("lab"), r.PathValue("vm"))
	if err != nil {
		writeErr(w, err)
		return
	}
	s.serveConsole(w, r, rec)
}

func (s *Server) serveConsole(w http.ResponseWriter, r *http.Request, rec VmRecord) {
	pty, restore, err := openConsole(rec)
	if err != nil {
		writeErr(w, err)
		return
	}
	conn, err := upgradeStream(w, r)
	if err != nil {
		restore()
		return
	}
	s.log.Info("console attached", "lab", rec.Lab, "vm", rec.ID, "remote", r.RemoteAddr)
	Splice(conn, &closeOnce{File: pty, close: restore})
	s.log.Info("console detached", "lab", rec.Lab, "vm", rec.ID)
}

type closeOnce struct {
	*os.File
	once  sync.Once
	close func()
}

func (c *closeOnce) Close() error { c.once.Do(c.close); return nil }

func (s *Server) handleTunnel(w http.ResponseWriter, r *http.Request) {
	lab, vm := r.PathValue("lab"), r.PathValue("vm")
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil || port < 1 || port > 65535 {
		http.Error(w, "port must be 1-65535", http.StatusBadRequest)
		return
	}
	_, rec, err := s.labs.VM(lab, vm)
	if err != nil {
		writeErr(w, err)
		return
	}
	// Dial before upgrading, so a closed port is an HTTP error the client can
	// report rather than a stream that ends at once.
	up, err := dialVM(r.Context(), rec.IP, port)
	if err != nil {
		http.Error(w, fmt.Sprintf("%s:%d: %v", vm, port, err), http.StatusBadGateway)
		return
	}
	conn, err := upgradeStream(w, r)
	if err != nil {
		up.Close()
		return
	}
	Splice(conn, up)
}

func (s *Server) handleEngine(w http.ResponseWriter, r *http.Request) {
	release := KernelRelease(KernelPath)
	info := EngineInfo{
		Version:     Version,
		Arch:        runtime.GOARCH,
		GuestKernel: release,
		KbuildImage: cmp.Or(s.config.KbuildImage, KbuildImageFor(release)),
		BaseImage:   BaseRootfs,
		MgmtSubnets: "10.0.1.0/24 - 10.0.254.0/24, one per lab",
		Capacity:    ComputeCapacity(s.state.List(), s.labs.Count()),
	}
	if release != "" && release != "installed" {
		info.KernelHeaders = "linux-headers-" + release
	}
	info.Artifacts.Count, info.Artifacts.Bytes = s.store.Usage()
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) handleArtifactHead(w http.ResponseWriter, r *http.Request) {
	d, err := ParseArtifactRef(r.PathValue("digest"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	size, err := s.store.Stat(d)
	if err != nil {
		w.WriteHeader(httpStatus(err))
		return
	}
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleArtifactPut(w http.ResponseWriter, r *http.Request) {
	d, err := ParseArtifactRef(r.PathValue("digest"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := s.store.Stat(d); err == nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	n, err := s.store.Put(d, http.MaxBytesReader(w, r.Body, MaxArtifactBytes))
	if err != nil {
		s.log.Warn("artifact upload failed", "artifact", d, "err", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.log.Info("artifact stored", "artifact", d, "bytes", n)
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) handleCreateLab(w http.ResponseWriter, r *http.Request) {
	var req CreateLabRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	st, err := s.labs.Create(req)
	if err != nil {
		s.log.Warn("lab rejected", "name", req.Name, "err", err)
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, st)
}

func (s *Server) handleListLabs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.labs.List())
}

func (s *Server) handleLabStatus(w http.ResponseWriter, r *http.Request) {
	st, err := s.labs.Status(r.PathValue("lab"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleDeleteLab(w http.ResponseWriter, r *http.Request) {
	if err := s.labs.Delete(r.PathValue("lab")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleLabTopology(w http.ResponseWriter, r *http.Request) {
	t, err := s.labs.Topology(r.PathValue("lab"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) handleSetMirror(w http.ResponseWriter, r *http.Request) {
	var req MirrorRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	lab, link := r.PathValue("lab"), r.PathValue("link")
	if err := s.labs.SetMirror(lab, link, req.To); err != nil {
		writeErr(w, err)
		return
	}
	s.log.Info("mirror set", "lab", lab, "link", link, "to", req.To)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleClearMirror(w http.ResponseWriter, r *http.Request) {
	if err := s.labs.ClearMirror(r.PathValue("lab"), r.PathValue("link")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSetFilter(w http.ResponseWriter, r *http.Request) {
	var f LinkFilter
	if !decodeJSON(w, r, &f) {
		return
	}
	lab, link := r.PathValue("lab"), r.PathValue("link")
	if err := s.labs.SetFilter(lab, link, f); err != nil {
		writeErr(w, err)
		return
	}
	s.log.Info("filter set", "lab", lab, "link", link, "filter", f)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleClearFilter(w http.ResponseWriter, r *http.Request) {
	if err := s.labs.ClearFilter(r.PathValue("lab"), r.PathValue("link")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMirrorStatus(w http.ResponseWriter, r *http.Request) {
	st, err := s.labs.MirrorStatus(r.PathValue("lab"), r.PathValue("link"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleSetDrop(w http.ResponseWriter, r *http.Request) {
	var req DropRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	lab, link := r.PathValue("lab"), r.PathValue("link")
	st, err := s.labs.SetDrop(lab, link, req)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.log.Info("drop armed", "lab", lab, "link", link, "rules", len(req.Rules), "once", req.Once)
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleDropStatus(w http.ResponseWriter, r *http.Request) {
	st, err := s.labs.DropStatus(r.PathValue("lab"), r.PathValue("link"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleClearDrop(w http.ResponseWriter, r *http.Request) {
	st, err := s.labs.ClearDrop(r.PathValue("lab"), r.PathValue("link"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}
