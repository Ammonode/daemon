package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ammonode/daemon/internal/config"
	"github.com/ammonode/daemon/internal/server"
)

func (r *Router) compressFiles(w http.ResponseWriter, req *http.Request, s *server.Server) {
	var body struct {
		Root  string   `json:"root"`
		Files []string `json:"files"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil || len(body.Files) == 0 {
		writeError(w, http.StatusUnprocessableEntity, "files are required")
		return
	}
	base, ok := r.resolve(w, s, body.Root)
	if !ok {
		return
	}
	rels := make([]string, 0, len(body.Files))
	for _, f := range body.Files {
		full, ok := r.resolve(w, s, path.Join(body.Root, f))
		if !ok {
			return
		}
		rel, _ := filepath.Rel(base, full)
		rels = append(rels, rel)
	}
	name := "archive-" + time.Now().UTC().Format("2006-01-02T150405") + ".tar.gz"
	dst := filepath.Join(base, name)
	size, err := server.CreateArchive(dst, base, rels, func(rel string) bool {
		return denied(s, path.Join(body.Root, filepath.ToSlash(rel)))
	})
	if err != nil {
		os.Remove(dst)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.UpdateDiskUsage()
	if !s.HasSpaceFor(0) {
		os.Remove(dst)
		s.UpdateDiskUsage()
		writeError(w, http.StatusConflict, server.ErrNotEnoughDisk.Error())
		return
	}
	st, _ := os.Stat(dst)
	writeJSON(w, http.StatusOK, map[string]interface{}{"name": name, "size": size, "modified": st.ModTime()})
}

func (r *Router) decompressFile(w http.ResponseWriter, req *http.Request, s *server.Server) {
	var body struct {
		Root string `json:"root"`
		File string `json:"file"`
	}
	_ = json.NewDecoder(req.Body).Decode(&body)
	src, ok := r.resolve(w, s, path.Join(body.Root, body.File))
	if !ok {
		return
	}
	dir, ok := r.resolve(w, s, body.Root)
	if !ok {
		return
	}
	err := server.ExtractArchive(src, s.Dir(), dir, s.RemainingSpace())
	s.UpdateDiskUsage()
	switch {
	case errors.Is(err, server.ErrUnsupportedArchive):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, server.ErrNotEnoughDisk):
		writeError(w, http.StatusConflict, "not enough disk space to extract this archive")
	case errors.Is(err, server.ErrBadPath):
		writeError(w, http.StatusBadRequest, "the archive contains files outside of its folder")
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusNoContent, nil)
	}
}

func (r *Router) chmodFiles(w http.ResponseWriter, req *http.Request, s *server.Server) {
	var body struct {
		Root  string `json:"root"`
		Files []struct {
			File string `json:"file"`
			Mode string `json:"mode"`
		} `json:"files"`
	}
	_ = json.NewDecoder(req.Body).Decode(&body)
	for _, f := range body.Files {
		mode, err := strconv.ParseUint(f.Mode, 8, 32)
		if err != nil || mode > 0o777 {
			writeError(w, http.StatusUnprocessableEntity, "mode must be octal, e.g. 644")
			return
		}
		full, ok := r.resolve(w, s, path.Join(body.Root, f.File))
		if !ok {
			return
		}
		if err := os.Chmod(full, os.FileMode(mode)); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusNoContent, nil)
}

// publicDialer refuses connections to loopback, private and link-local
// addresses so "pull from URL" cannot reach internal services.
var publicDialer = &net.Dialer{
	Timeout: 15 * time.Second,
	Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
			return fmt.Errorf("downloads from %s are not allowed", host)
		}
		return nil
	},
}

var pullClient = &http.Client{
	Timeout:   30 * time.Minute,
	Transport: &http.Transport{DialContext: publicDialer.DialContext, TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: 30 * time.Second},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		return nil
	},
}

func (r *Router) pullFile(w http.ResponseWriter, req *http.Request, s *server.Server) {
	var body struct {
		URL       string `json:"url"`
		Directory string `json:"directory"`
		Filename  string `json:"filename"`
	}
	_ = json.NewDecoder(req.Body).Decode(&body)
	u, err := url.Parse(body.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		writeError(w, http.StatusUnprocessableEntity, "a valid http(s) URL is required")
		return
	}
	if _, ok := r.resolve(w, s, body.Directory); !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	dl, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	dl.Header.Set("User-Agent", "Ammonode-Daemon")
	res, err := pullClient.Do(dl)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		writeError(w, http.StatusBadGateway, "remote server returned "+res.Status)
		return
	}
	name := body.Filename
	if name == "" {
		if _, params, err := mime.ParseMediaType(res.Header.Get("Content-Disposition")); err == nil && params["filename"] != "" {
			name = params["filename"]
		} else {
			name = path.Base(u.Path)
		}
	}
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	if name == "" || name == "." || name == "/" {
		name = "download"
	}
	if res.ContentLength > 0 && !s.HasSpaceFor(res.ContentLength) {
		writeError(w, http.StatusConflict, server.ErrNotEnoughDisk.Error())
		return
	}
	target, ok := r.resolve(w, s, path.Join(body.Directory, name))
	if !ok {
		return
	}
	f, err := os.Create(target)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	limit := s.RemainingSpace()
	var src io.Reader = res.Body
	if limit >= 0 {
		src = io.LimitReader(res.Body, limit+1)
	}
	n, err := io.Copy(f, src)
	f.Close()
	if err == nil && limit >= 0 && n > limit {
		err = server.ErrNotEnoughDisk
	}
	if err != nil {
		os.Remove(target)
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	s.UpdateDiskUsage()
	writeJSON(w, http.StatusOK, map[string]interface{}{"name": name, "size": n})
}

// uploadFiles accepts multipart uploads straight from the browser using a
// short lived token issued by the panel.
func (r *Router) uploadFiles(w http.ResponseWriter, req *http.Request) {
	c, err := parseToken(r.cfg.Token, req.URL.Query().Get("token"), &UploadClaims{})
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	s, ok := r.manager.Get(c.ServerUUID)
	if !ok {
		writeError(w, http.StatusNotFound, "server not found")
		return
	}
	max := int64(config.Get().API.UploadLimit) * 1024 * 1024
	req.Body = http.MaxBytesReader(w, req.Body, max+1024*1024)
	reader, err := req.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "expected a multipart upload")
		return
	}
	directory := req.URL.Query().Get("directory")
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if part.FileName() == "" {
			continue
		}
		name := filepath.Base(strings.ReplaceAll(part.FileName(), "\\", "/"))
		rel := path.Join(directory, name)
		if denied(s, rel) {
			writeError(w, http.StatusForbidden, name+" is not allowed")
			return
		}
		target, err := server.SafePath(s.Dir(), rel)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		_ = os.MkdirAll(filepath.Dir(target), 0o755)
		f, err := os.Create(target)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		limit := s.RemainingSpace()
		var src io.Reader = part
		if limit >= 0 {
			src = io.LimitReader(part, limit+1)
		}
		n, err := io.Copy(f, src)
		f.Close()
		if err == nil && limit >= 0 && n > limit {
			err = server.ErrNotEnoughDisk
		}
		if err != nil {
			os.Remove(target)
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		s.UpdateDiskUsage()
	}
	writeJSON(w, http.StatusNoContent, nil)
}
