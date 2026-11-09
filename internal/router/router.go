// Package router exposes the daemon HTTP API used by the panel and browsers.
package router

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ammonode/daemon/internal/config"
	"github.com/ammonode/daemon/internal/server"
	"github.com/ammonode/daemon/internal/system"
)

type Router struct {
	cfg     *config.Configuration
	manager *server.Manager
}

func New(cfg *config.Configuration, m *server.Manager) http.Handler {
	r := &Router{cfg: cfg, manager: m}
	mux := http.NewServeMux()

	// Browser endpoints authenticate with a short lived JWT signed by the panel.
	mux.HandleFunc("GET /api/servers/{uuid}/ws", r.websocket)
	mux.HandleFunc("GET /download/file", r.downloadFile)
	mux.HandleFunc("POST /upload/file", r.uploadFiles)
	mux.HandleFunc("GET /download/backup", r.downloadBackup)
	mux.HandleFunc("POST /api/transfers/receive", r.receiveTransfer)
	mux.HandleFunc("POST /api/transfers/receive/backup/{backup}", r.receiveTransferBackup)

	// Panel endpoints authenticate with the node token.
	panel := http.NewServeMux()
	panel.HandleFunc("GET /api/system", r.systemInfo)
	panel.HandleFunc("GET /api/servers", r.listServers)
	panel.HandleFunc("POST /api/servers", r.createServer)
	panel.HandleFunc("GET /api/servers/{uuid}", r.withServer(r.serverDetails))
	panel.HandleFunc("DELETE /api/servers/{uuid}", r.withServer(r.deleteServer))
	panel.HandleFunc("POST /api/servers/{uuid}/sync", r.withServer(r.syncServer))
	panel.HandleFunc("POST /api/servers/{uuid}/power", r.withServer(r.power))
	panel.HandleFunc("POST /api/servers/{uuid}/commands", r.withServer(r.commands))
	panel.HandleFunc("POST /api/servers/{uuid}/install", r.withServer(r.install))
	panel.HandleFunc("POST /api/servers/{uuid}/reinstall", r.withServer(r.reinstall))
	panel.HandleFunc("GET /api/servers/{uuid}/logs", r.withServer(r.logs))
	panel.HandleFunc("GET /api/servers/{uuid}/files/list-directory", r.withServer(r.listDirectory))
	panel.HandleFunc("GET /api/servers/{uuid}/files/contents", r.withServer(r.fileContents))
	panel.HandleFunc("POST /api/servers/{uuid}/files/write", r.withServer(r.writeFile))
	panel.HandleFunc("PUT /api/servers/{uuid}/files/rename", r.withServer(r.renameFiles))
	panel.HandleFunc("POST /api/servers/{uuid}/files/create-directory", r.withServer(r.createDirectory))
	panel.HandleFunc("POST /api/servers/{uuid}/files/delete", r.withServer(r.deleteFiles))
	panel.HandleFunc("POST /api/servers/{uuid}/files/compress", r.withServer(r.compressFiles))
	panel.HandleFunc("POST /api/servers/{uuid}/files/decompress", r.withServer(r.decompressFile))
	panel.HandleFunc("POST /api/servers/{uuid}/files/chmod", r.withServer(r.chmodFiles))
	panel.HandleFunc("POST /api/servers/{uuid}/files/pull", r.withServer(r.pullFile))
	panel.HandleFunc("POST /api/servers/{uuid}/backup", r.withServer(r.createBackup))
	panel.HandleFunc("POST /api/transfers", r.prepareTransfer)
	panel.HandleFunc("POST /api/servers/{uuid}/transfer", r.withServer(r.sendTransfer))
	panel.HandleFunc("DELETE /api/servers/{uuid}/backup/{backup}", r.withServer(r.deleteBackup))
	panel.HandleFunc("POST /api/servers/{uuid}/backup/{backup}/restore", r.withServer(r.restoreBackup))
	mux.Handle("/api/", r.requireToken(panel))

	return r.recover(r.cors(mux))
}

type ctxKey string

const serverKey ctxKey = "server"

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]interface{}{"error": map[string]string{"message": msg}})
}

func (r *Router) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				writeError(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, req)
	})
}

func (r *Router) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		origin := req.Header.Get("Origin")
		if origin != "" && (origin == strings.TrimRight(r.cfg.Remote, "/") || contains(r.cfg.API.TrustedOrigins, origin)) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Accept, Authorization, Content-Type")
			w.Header().Set("Access-Control-Max-Age", "7200")
		}
		if req.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, req)
	})
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func (r *Router) requireToken(next http.Handler) http.Handler {
	expected := "Bearer " + r.cfg.Token
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !constantTimeEqual(req.Header.Get("Authorization"), expected) {
			writeError(w, http.StatusUnauthorized, "invalid authorization token")
			return
		}
		next.ServeHTTP(w, req)
	})
}

func (r *Router) withServer(h func(http.ResponseWriter, *http.Request, *server.Server)) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		s, ok := r.manager.Get(req.PathValue("uuid"))
		if !ok {
			writeError(w, http.StatusNotFound, "server not found on this node")
			return
		}
		h(w, req, s)
	}
}

func (r *Router) systemInfo(w http.ResponseWriter, req *http.Request) {
	info, err := system.Info(req.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func serverView(s *server.Server) map[string]interface{} {
	st := s.Stats()
	st.State = s.State()
	return map[string]interface{}{
		"uuid":          s.ID(),
		"state":         s.State(),
		"is_installing": s.IsInstalling(),
		"is_suspended":  s.Config().Suspended,
		"utilization":   st,
	}
}

func (r *Router) listServers(w http.ResponseWriter, _ *http.Request) {
	out := []map[string]interface{}{}
	for _, s := range r.manager.All() {
		out = append(out, serverView(s))
	}
	writeJSON(w, http.StatusOK, out)
}

func (r *Router) serverDetails(w http.ResponseWriter, _ *http.Request, s *server.Server) {
	writeJSON(w, http.StatusOK, serverView(s))
}

// createServer registers a server from the panel and runs its installer in the background.
func (r *Router) createServer(w http.ResponseWriter, req *http.Request) {
	var body struct {
		UUID        string `json:"uuid"`
		StartOnDone bool   `json:"start_on_completion"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.UUID == "" {
		writeError(w, http.StatusUnprocessableEntity, "uuid is required")
		return
	}
	cfg, err := r.manager.Panel().Server(req.Context(), body.UUID)
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not fetch server configuration from panel: "+err.Error())
		return
	}
	s := r.manager.Add(cfg)
	go func() {
		ctx := context.Background()
		if err := s.Install(ctx, false); err == nil && body.StartOnDone {
			_ = s.Start(ctx)
		}
	}()
	writeJSON(w, http.StatusAccepted, nil)
}

func (r *Router) deleteServer(w http.ResponseWriter, req *http.Request, s *server.Server) {
	r.manager.Remove(s.ID())
	_ = os.RemoveAll(incomingBackups(s.ID()))
	go func() { _ = s.Destroy(context.Background()) }()
	writeJSON(w, http.StatusNoContent, nil)
}

func (r *Router) syncServer(w http.ResponseWriter, req *http.Request, s *server.Server) {
	cfg, err := r.manager.Panel().Server(req.Context(), s.ID())
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	s.SetConfig(cfg)
	if cfg.Suspended && s.State() != server.StateOffline {
		go func() { _ = s.WaitForStop(context.Background(), time.Minute, true) }()
	}
	writeJSON(w, http.StatusNoContent, nil)
}

// Power runs the action asynchronously so slow image pulls do not block the panel.
func Power(s *server.Server, action string) error {
	ctx := context.Background()
	switch action {
	case "start":
		return s.Start(ctx)
	case "stop":
		return s.Stop(ctx)
	case "restart":
		return s.Restart(ctx)
	case "kill":
		return s.Kill(ctx)
	}
	return errors.New("invalid power action")
}

func (r *Router) power(w http.ResponseWriter, req *http.Request, s *server.Server) {
	var body struct {
		Action string `json:"action"`
	}
	_ = json.NewDecoder(req.Body).Decode(&body)
	switch body.Action {
	case "start", "stop", "restart", "kill":
	default:
		writeError(w, http.StatusUnprocessableEntity, "action must be one of start, stop, restart, kill")
		return
	}
	if body.Action == "start" && s.Config().Suspended {
		writeError(w, http.StatusBadRequest, server.ErrSuspended.Error())
		return
	}
	go func() {
		if err := Power(s, body.Action); err != nil {
			s.DaemonMessage("Power action failed: " + err.Error())
		}
	}()
	writeJSON(w, http.StatusAccepted, nil)
}

func (r *Router) commands(w http.ResponseWriter, req *http.Request, s *server.Server) {
	var body struct {
		Commands []string `json:"commands"`
	}
	_ = json.NewDecoder(req.Body).Decode(&body)
	for _, c := range body.Commands {
		if err := s.SendCommand(c); err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusNoContent, nil)
}

func (r *Router) install(w http.ResponseWriter, _ *http.Request, s *server.Server) {
	go func() { _ = s.Install(context.Background(), false) }()
	writeJSON(w, http.StatusAccepted, nil)
}

func (r *Router) reinstall(w http.ResponseWriter, _ *http.Request, s *server.Server) {
	if s.IsInstalling() {
		writeError(w, http.StatusConflict, server.ErrInstalling.Error())
		return
	}
	go func() { _ = s.Install(context.Background(), true) }()
	writeJSON(w, http.StatusAccepted, nil)
}

func (r *Router) logs(w http.ResponseWriter, _ *http.Request, s *server.Server) {
	writeJSON(w, http.StatusOK, map[string]interface{}{"data": s.Logs.Lines()})
}
