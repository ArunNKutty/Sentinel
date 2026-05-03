package plasma

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"os"
)

type Server struct {
	state *State
	log   *slog.Logger
}

func NewServer(state *State, log *slog.Logger) *Server {
	return &Server{state: state, log: log}
}

func (s *Server) Listen(socket string) (net.Listener, *http.Server, error) {
	_ = os.Remove(socket)
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return nil, nil, err
	}
	if err := os.Chmod(socket, 0o660); err != nil {
		ln.Close()
		return nil, nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/vms", s.handleLaunch)
	mux.HandleFunc("GET /v1/vms", s.handleList)

	return ln, &http.Server{Handler: mux}, nil
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

	rec, err := Launch(req.Group, s.state)
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
