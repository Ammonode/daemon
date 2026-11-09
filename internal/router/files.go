package router

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ammonode/daemon/internal/server"
)

type fileStat struct {
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	Mode      string    `json:"mode"`
	ModeBits  string    `json:"mode_bits"`
	Directory bool      `json:"directory"`
	File      bool      `json:"file"`
	Symlink   bool      `json:"symlink"`
	Mime      string    `json:"mime"`
	Created   time.Time `json:"created"`
	Modified  time.Time `json:"modified"`
}

func statOf(name string, info os.FileInfo) fileStat {
	mime := "application/octet-stream"
	if info.IsDir() {
		mime = "inode/directory"
	} else if ext := strings.ToLower(filepath.Ext(name)); ext != "" {
		switch ext {
		case ".txt", ".log", ".properties", ".cfg", ".conf", ".ini", ".yml", ".yaml", ".json", ".toml", ".sh", ".md", ".xml", ".lua", ".js", ".ts", ".py", ".env":
			mime = "text/plain"
		case ".jar":
			mime = "application/java-archive"
		case ".zip":
			mime = "application/zip"
		case ".gz", ".tgz":
			mime = "application/gzip"
		}
	}
	return fileStat{
		Name: name, Size: info.Size(), Mode: info.Mode().String(), ModeBits: strings.TrimPrefix(info.Mode().Perm().String(), "-"),
		Directory: info.IsDir(), File: info.Mode().IsRegular(), Symlink: info.Mode()&os.ModeSymlink != 0,
		Mime: mime, Created: info.ModTime(), Modified: info.ModTime(),
	}
}

func denied(s *server.Server, p string) bool {
	clean := strings.TrimPrefix(path.Clean("/"+p), "/")
	for _, pattern := range s.Config().Egg.FileDenylist {
		pattern = strings.TrimPrefix(pattern, "/")
		if ok, _ := path.Match(pattern, clean); ok {
			return true
		}
		if ok, _ := path.Match(pattern, path.Base(clean)); ok {
			return true
		}
	}
	return false
}

func (r *Router) resolve(w http.ResponseWriter, s *server.Server, p string) (string, bool) {
	full, err := server.SafePath(s.Dir(), p)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return "", false
	}
	return full, true
}

func (r *Router) listDirectory(w http.ResponseWriter, req *http.Request, s *server.Server) {
	dir, ok := r.resolve(w, s, req.URL.Query().Get("directory"))
	if !ok {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		writeError(w, http.StatusNotFound, "directory not found")
		return
	}
	out := make([]fileStat, 0, len(entries))
	for _, e := range entries {
		if info, err := e.Info(); err == nil {
			out = append(out, statOf(e.Name(), info))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Directory != out[j].Directory {
			return out[i].Directory
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	writeJSON(w, http.StatusOK, out)
}

func (r *Router) fileContents(w http.ResponseWriter, req *http.Request, s *server.Server) {
	p := req.URL.Query().Get("file")
	if denied(s, p) {
		writeError(w, http.StatusForbidden, "this file cannot be read")
		return
	}
	full, ok := r.resolve(w, s, p)
	if !ok {
		return
	}
	f, err := os.Open(full)
	if err != nil {
		writeError(w, http.StatusNotFound, "file not found")
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	if st.IsDir() {
		writeError(w, http.StatusBadRequest, "cannot read a directory")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Mime-Type", statOf(st.Name(), st).Mime)
	_, _ = io.Copy(w, io.LimitReader(f, 25*1024*1024))
}

func (r *Router) writeFile(w http.ResponseWriter, req *http.Request, s *server.Server) {
	p := req.URL.Query().Get("file")
	if denied(s, p) {
		writeError(w, http.StatusForbidden, "this file cannot be modified")
		return
	}
	full, ok := r.resolve(w, s, p)
	if !ok {
		return
	}
	if req.ContentLength > 0 && !s.HasSpaceFor(req.ContentLength) {
		writeError(w, http.StatusConflict, server.ErrNotEnoughDisk.Error())
		return
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	f, err := os.OpenFile(full, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer f.Close()
	if _, err := io.Copy(f, io.LimitReader(req.Body, int64(r.cfg.API.UploadLimit)*1024*1024)); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	go s.UpdateDiskUsage()
	writeJSON(w, http.StatusNoContent, nil)
}

func (r *Router) renameFiles(w http.ResponseWriter, req *http.Request, s *server.Server) {
	var body struct {
		Root  string `json:"root"`
		Files []struct {
			From string `json:"from"`
			To   string `json:"to"`
		} `json:"files"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid body")
		return
	}
	for _, f := range body.Files {
		from, ok := r.resolve(w, s, path.Join(body.Root, f.From))
		if !ok {
			return
		}
		to, ok := r.resolve(w, s, path.Join(body.Root, f.To))
		if !ok {
			return
		}
		if denied(s, path.Join(body.Root, f.To)) {
			writeError(w, http.StatusForbidden, "target path is not allowed")
			return
		}
		_ = os.MkdirAll(filepath.Dir(to), 0o755)
		if err := os.Rename(from, to); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusNoContent, nil)
}

func (r *Router) createDirectory(w http.ResponseWriter, req *http.Request, s *server.Server) {
	var body struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}
	_ = json.NewDecoder(req.Body).Decode(&body)
	full, ok := r.resolve(w, s, path.Join(body.Path, body.Name))
	if !ok {
		return
	}
	if err := os.MkdirAll(full, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

func (r *Router) deleteFiles(w http.ResponseWriter, req *http.Request, s *server.Server) {
	var body struct {
		Root  string   `json:"root"`
		Files []string `json:"files"`
	}
	_ = json.NewDecoder(req.Body).Decode(&body)
	for _, f := range body.Files {
		full, ok := r.resolve(w, s, path.Join(body.Root, f))
		if !ok {
			return
		}
		if full == s.Dir() {
			writeError(w, http.StatusBadRequest, "cannot delete the root directory")
			return
		}
		if err := os.RemoveAll(full); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	go s.UpdateDiskUsage()
	writeJSON(w, http.StatusNoContent, nil)
}
