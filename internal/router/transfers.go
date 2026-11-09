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

// incomingBackups holds backups received during a transfer until the server
// archive arrives. It sits inside the backup directory so moving the files
// into place is a rename on the same filesystem.
func incomingBackups(serverUUID string) string {
	return filepath.Join(config.Get().System.BackupDir, ".incoming-"+filepath.Base(serverUUID))
}

// receiveToFile streams a request body to dst and checks the X-Checksum header.
func receiveToFile(req *http.Request, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), req.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(dst)
		return errors.New("upload interrupted: " + err.Error())
	}
	if want := req.Header.Get("X-Checksum"); want != "" && want != hex.EncodeToString(h.Sum(nil)) {
		_ = os.Remove(dst)
		return errors.New("checksum mismatch, the archive was corrupted in transit")
	}
	return nil
}

// receiveTransferBackup (target node): store one local backup of the incoming server.
func (r *Router) receiveTransferBackup(w http.ResponseWriter, req *http.Request) {
	c, err := parseToken(r.cfg.Token, bearer(req), &TransferClaims{})
	if err != nil || !c.Transfer {
		writeError(w, http.StatusUnauthorized, "invalid transfer token")
		return
	}
	if _, ok := r.manager.Get(c.ServerUUID); !ok {
		writeError(w, http.StatusNotFound, "the transfer was not prepared on this node")
		return
	}
	id := req.PathValue("backup")
	if !validUUID(id) {
		writeError(w, http.StatusUnprocessableEntity, "invalid backup id")
		return
	}
	if err := receiveToFile(req, filepath.Join(incomingBackups(c.ServerUUID), id+".tar.gz")); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// adoptIncomingBackups moves received backups into the backup directory.
func adoptIncomingBackups(serverUUID string) error {
	dir := incomingBackups(serverUUID)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.Rename(filepath.Join(dir, e.Name()), filepath.Join(filepath.Dir(dir), e.Name())); err != nil {
			return err
		}
	}
	return os.Remove(dir)
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
		_ = os.RemoveAll(incomingBackups(s.ID()))
		go r.reportTransfer(s.ID(), false, msg)
	}
	tmp := filepath.Join(config.Get().System.TmpDirectory, "transfer-"+s.ID()+".tar.gz")
	_ = os.MkdirAll(filepath.Dir(tmp), 0o700)
	defer os.Remove(tmp)
	if err := receiveToFile(req, tmp); err != nil {
		fail(http.StatusBadRequest, err.Error())
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
	if err := adoptIncomingBackups(s.ID()); err != nil {
		fail(http.StatusInternalServerError, "could not store backups: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	go r.reportTransfer(s.ID(), true, "")
}

// sendTransfer (source node): stop, archive and push the server to the target.
func (r *Router) sendTransfer(w http.ResponseWriter, req *http.Request, s *server.Server) {
	var body struct {
		URL   string `json:"url"`
		Token string `json:"token"`
		// Local backups to copy to the target before the server files.
		Backups []string `json:"backups"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.URL == "" || body.Token == "" {
		writeError(w, http.StatusUnprocessableEntity, "url and token are required")
		return
	}
	go func() {
		if err := pushServer(s, body.URL, body.Token, body.Backups); err != nil {
			s.DaemonMessage("Transfer failed: " + err.Error())
			r.reportTransfer(s.ID(), false, err.Error())
		}
	}()
	writeJSON(w, http.StatusAccepted, nil)
}

func pushServer(s *server.Server, url, token string, backups []string) error {
	ctx := context.Background()
	s.DaemonMessage("Transferring this server to another node. It will be offline until the transfer finishes.")
	if err := s.WaitForStop(ctx, 2*time.Minute, true); err != nil {
		return fmt.Errorf("stop server: %w", err)
	}
	for i, id := range backups {
		if !validUUID(id) {
			continue
		}
		p := server.BackupPath(id)
		if _, err := os.Stat(p); os.IsNotExist(err) {
			s.DaemonMessage("Backup " + id + " is missing on this node, skipped.")
			continue
		}
		s.DaemonMessage(fmt.Sprintf("Copying backup %d of %d to the new node.", i+1, len(backups)))
		if err := pushFile(p, url+"/backup/"+id, token); err != nil {
			return fmt.Errorf("backup %s: %w", id, err)
		}
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
	if err := pushFile(tmp, url, token); err != nil {
		return err
	}
	s.DaemonMessage("Archive delivered to the new node.")
	return nil
}

// pushFile uploads a file with its checksum to a transfer endpoint of the target node.
func pushFile(path, url, token string) error {
	f, err := os.Open(path)
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
	return nil
}

func bearer(req *http.Request) string {
	h := req.Header.Get("Authorization")
	if len(h) > 7 && h[:7] == "Bearer " {
		return h[7:]
	}
	return ""
}

// validUUID accepts the canonical 36 character form only, so ids are safe in file names.
func validUUID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, c := range id {
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return false
			}
		case (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F'):
		default:
			return false
		}
	}
	return true
}
