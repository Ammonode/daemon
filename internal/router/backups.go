package router

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/ammonode/daemon/internal/server"
)

// BackupClaims authorises downloading one backup.
type BackupClaims struct {
	jwt.RegisteredClaims
	ServerUUID string `json:"server_uuid"`
	BackupUUID string `json:"backup_uuid"`
}

func (r *Router) createBackup(w http.ResponseWriter, req *http.Request, s *server.Server) {
	var body struct {
		UUID   string `json:"uuid"`
		Ignore string `json:"ignore"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.UUID == "" {
		writeError(w, http.StatusUnprocessableEntity, "uuid is required")
		return
	}
	go s.Backup(body.UUID, body.Ignore)
	writeJSON(w, http.StatusAccepted, nil)
}

func (r *Router) deleteBackup(w http.ResponseWriter, req *http.Request, _ *server.Server) {
	if err := os.Remove(server.BackupPath(req.PathValue("backup"))); err != nil && !os.IsNotExist(err) {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

func (r *Router) restoreBackup(w http.ResponseWriter, req *http.Request, s *server.Server) {
	var body struct {
		Truncate bool `json:"truncate"`
	}
	_ = json.NewDecoder(req.Body).Decode(&body)
	backup := req.PathValue("backup")
	if _, err := os.Stat(server.BackupPath(backup)); err != nil {
		writeError(w, http.StatusNotFound, "backup not found on this node")
		return
	}
	go func() {
		err := s.Restore(backup, body.Truncate)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = r.manager.Panel().SetBackupStatus(ctx, backup+"/restore", map[string]bool{"successful": err == nil})
	}()
	writeJSON(w, http.StatusAccepted, nil)
}

func (r *Router) downloadBackup(w http.ResponseWriter, req *http.Request) {
	c, err := parseToken(r.cfg.Token, req.URL.Query().Get("token"), &BackupClaims{})
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	if _, ok := r.manager.Get(c.ServerUUID); !ok {
		writeError(w, http.StatusNotFound, "server not found")
		return
	}
	f, err := os.Open(server.BackupPath(c.BackupUUID))
	if err != nil {
		writeError(w, http.StatusNotFound, "backup not found")
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("Content-Disposition", `attachment; filename="`+c.BackupUUID+`.tar.gz"`)
	w.Header().Set("Content-Type", "application/gzip")
	http.ServeContent(w, req, st.Name(), st.ModTime(), f)
}
