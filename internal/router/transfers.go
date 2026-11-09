package router

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/ammonode/daemon/internal/config"
	"github.com/ammonode/daemon/internal/server"
)

// TransferClaims authorises one archive upload into this node.
type TransferClaims struct {
	jwt.RegisteredClaims
	ServerUUID string `json:"server_uuid"`
	Transfer   bool   `json:"transfer"`
}

func (r *Router) reportTransfer(uuid string, ok bool, msg string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := r.manager.Panel().SetTransferStatus(ctx, uuid, ok, msg); err != nil {
		slog.Error("could not report transfer status", "server", uuid, "error", err)
	}
}

// prepareTransfer (target node): register the incoming server so uploads can be accepted.
func (r *Router) prepareTransfer(w http.ResponseWriter, req *http.Request) {
	var body struct {
		UUID string `json:"server_uuid"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.UUID == "" {
		writeError(w, http.StatusUnprocessableEntity, "server_uuid is required")
		return
	}
	if _, ok := r.manager.Get(body.UUID); ok {
		writeError(w, http.StatusConflict, "this server already exists on the node")
		return
	}
	cfg, err := r.manager.Panel().Server(req.Context(), body.UUID)
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not fetch server configuration: "+err.Error())
		return
	}
	s := r.manager.Add(cfg)
	if err := os.MkdirAll(s.Dir(), 0o700); err != nil {
		r.manager.Remove(cfg.UUID)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, nil)
}

// receiveTransfer (target node): stream the archive to disk, verify, extract.
func (r *Router) receiveTransfer(w http.ResponseWriter, req *http.Request) {
	c, err := parseToken(r.cfg.Token, bearer(req), &TransferClaims{})
	if err != nil || !c.Transfer {
		writeError(w, http.StatusUnauthorized, "invalid transfer token")
		return
	}
	s, ok := r.manager.Get(c.ServerUUID)
	if !ok {
		writeError(w, http.StatusNotFound, "the transfer was not prepared on this node")
		return
	}
	fail := func(status int, msg string) {
		writeError(w, status, msg)
		r.manager.Remove(s.ID())
		_ = os.RemoveAll(s.Dir())
		go r.reportTransfer(s.ID(), false, msg)
	}
	tmp := filepath.Join(config.Get().System.TmpDirectory, "transfer-"+s.ID()+".tar.gz")
	_ = os.MkdirAll(filepath.Dir(tmp), 0o700)
	defer os.Remove(tmp)
	f, err := os.Create(tmp)
	if err != nil {
		fail(http.StatusInternalServerError, err.Error())
		return
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), req.Body)
	f.Close()
	if err != nil {
		fail(http.StatusBadRequest, "upload interrupted: "+err.Error())
		return
	}
	if want := req.Header.Get("X-Checksum"); want != "" && want != hex.EncodeToString(h.Sum(nil)) {
		fail(http.StatusBadRequest, "checksum mismatch, the archive was corrupted in transit")
		return
	}
	if err := server.ExtractArchive(tmp, s.Dir(), s.Dir(), s.RemainingSpace()); err != nil {
		fail(http.StatusInternalServerError, "extract failed: "+err.Error())
		return
	}
	cfg := config.Get()
	_ = filepath.Walk(s.Dir(), func(p string, _ os.FileInfo, err error) error {
		if err == nil {
			_ = os.Lchown(p, cfg.System.User.UID, cfg.System.User.GID)
		}
		return nil
	})
	s.UpdateDiskUsage()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	go r.reportTransfer(s.ID(), true, "")
}

// sendTransfer (source node): stop, archive and push the server to the target.
func (r *Router) sendTransfer(w http.ResponseWriter, req *http.Request, s *server.Server) {
	var body struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.URL == "" || body.Token == "" {
		writeError(w, http.StatusUnprocessableEntity, "url and token are required")
		return
	}
	go func() {
		if err := pushServer(s, body.URL, body.Token); err != nil {
			s.DaemonMessage("Transfer failed: " + err.Error())
			r.reportTransfer(s.ID(), false, err.Error())
		}
	}()
	writeJSON(w, http.StatusAccepted, nil)
}

func pushServer(s *server.Server, url, token string) error {
	ctx := context.Background()
	s.DaemonMessage("Transferring this server to another node. It will be offline until the transfer finishes.")
	if err := s.WaitForStop(ctx, 2*time.Minute, true); err != nil {
		return fmt.Errorf("stop server: %w", err)
	}
	tmp := filepath.Join(config.Get().System.TmpDirectory, "outgoing-"+s.ID()+".tar.gz")
	_ = os.MkdirAll(filepath.Dir(tmp), 0o700)
	defer os.Remove(tmp)
	entries, err := os.ReadDir(s.Dir())
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if _, err := server.CreateArchive(tmp, s.Dir(), names, nil); err != nil {
		return fmt.Errorf("archive: %w", err)
	}
	f, err := os.Open(tmp)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	st, _ := f.Stat()
	req, err := http.NewRequest(http.MethodPost, url, f)
	if err != nil {
		return err
	}
	req.ContentLength = st.Size()
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/gzip")
	req.Header.Set("X-Checksum", hex.EncodeToString(h.Sum(nil)))
	res, err := (&http.Client{Timeout: 12 * time.Hour}).Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.NewDecoder(res.Body).Decode(&e)
		return errors.New("target node refused the archive: " + e.Error.Message)
	}
	s.DaemonMessage("Archive delivered to the new node.")
	return nil
}

func bearer(req *http.Request) string {
	h := req.Header.Get("Authorization")
	if len(h) > 7 && h[:7] == "Bearer " {
		return h[7:]
	}
	return ""
}
