package sftp

import (
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/pkg/sftp"

	"github.com/ammonode/daemon/internal/config"
	"github.com/ammonode/daemon/internal/server"
)

// handler implements pkg/sftp request handlers jailed to one server.
type handler struct {
	server      *server.Server
	permissions []string
	readOnly    bool
}

func (h *handler) can(p string) bool {
	for _, x := range h.permissions {
		if x == "*" || x == p {
			return true
		}
	}
	return false
}

func (h *handler) denied(p string) bool {
	clean := strings.TrimPrefix(path.Clean("/"+p), "/")
	for _, pattern := range h.server.Config().Egg.FileDenylist {
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

func (h *handler) resolve(p string) (string, error) {
	full, err := server.SafePath(h.server.Dir(), p)
	if err != nil {
		return "", sftp.ErrSSHFxPermissionDenied
	}
	return full, nil
}

func (h *handler) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	if !h.can("file.read-content") || h.denied(r.Filepath) {
		return nil, sftp.ErrSSHFxPermissionDenied
	}
	full, err := h.resolve(r.Filepath)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, sftp.ErrSSHFxNoSuchFile
	}
	return f, nil
}

// quotaWriter stops writes once the server runs out of disk space.
type quotaWriter struct {
	f      *os.File
	server *server.Server
	start  int64
}

func (q *quotaWriter) WriteAt(p []byte, off int64) (int, error) {
	end := off + int64(len(p))
	if end > q.start && !q.server.HasSpaceFor(end-q.start) {
		return 0, errors.New("not enough disk space")
	}
	return q.f.WriteAt(p, off)
}

func (q *quotaWriter) Close() error {
	err := q.f.Close()
	go q.server.UpdateDiskUsage()
	return err
}

func (h *handler) Filewrite(r *sftp.Request) (io.WriterAt, error) {
	if h.readOnly || h.denied(r.Filepath) {
		return nil, sftp.ErrSSHFxPermissionDenied
	}
	full, err := h.resolve(r.Filepath)
	if err != nil {
		return nil, err
	}
	st, statErr := os.Stat(full)
	if (statErr != nil && !h.can("file.create")) || (statErr == nil && !h.can("file.update")) {
		return nil, sftp.ErrSSHFxPermissionDenied
	}
	var size int64
	if statErr == nil {
		size = st.Size()
	}
	_ = os.MkdirAll(filepath.Dir(full), 0o755)
	flags := os.O_CREATE | os.O_WRONLY
	if r.Pflags().Trunc {
		flags |= os.O_TRUNC
		size = 0
	}
	f, err := os.OpenFile(full, flags, 0o644)
	if err != nil {
		return nil, sftp.ErrSSHFxFailure
	}
	cfg := config.Get()
	_ = f.Chown(cfg.System.User.UID, cfg.System.User.GID)
	return &quotaWriter{f: f, server: h.server, start: size}, nil
}

func (h *handler) Filecmd(r *sftp.Request) error {
	if h.readOnly {
		return sftp.ErrSSHFxPermissionDenied
	}
	full, err := h.resolve(r.Filepath)
	if err != nil {
		return err
	}
	switch r.Method {
	case "Setstat":
		if !h.can("file.update") {
			return sftp.ErrSSHFxPermissionDenied
		}
		attrs := r.Attributes()
		if r.AttrFlags().Permissions {
			return os.Chmod(full, attrs.FileMode().Perm())
		}
		return nil
	case "Rename":
		if !h.can("file.update") || h.denied(r.Target) {
			return sftp.ErrSSHFxPermissionDenied
		}
		target, err := h.resolve(r.Target)
		if err != nil {
			return err
		}
		return os.Rename(full, target)
	case "Rmdir", "Remove":
		if !h.can("file.delete") || full == h.server.Dir() {
			return sftp.ErrSSHFxPermissionDenied
		}
		err := os.RemoveAll(full)
		go h.server.UpdateDiskUsage()
		return err
	case "Mkdir":
		if !h.can("file.create") {
			return sftp.ErrSSHFxPermissionDenied
		}
		return os.MkdirAll(full, 0o755)
	case "Symlink", "Link":
		// Links could point outside of the server directory.
		return sftp.ErrSSHFxOpUnsupported
	}
	return sftp.ErrSSHFxOpUnsupported
}

type listerat []os.FileInfo

func (l listerat) ListAt(out []os.FileInfo, offset int64) (int, error) {
	if offset >= int64(len(l)) {
		return 0, io.EOF
	}
	n := copy(out, l[offset:])
	if n < len(out) {
		return n, io.EOF
	}
	return n, nil
}

func (h *handler) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	if !h.can("file.read") {
		return nil, sftp.ErrSSHFxPermissionDenied
	}
	full, err := h.resolve(r.Filepath)
	if err != nil {
		return nil, err
	}
	switch r.Method {
	case "List":
		entries, err := os.ReadDir(full)
		if err != nil {
			return nil, sftp.ErrSSHFxNoSuchFile
		}
		out := make([]os.FileInfo, 0, len(entries))
		for _, e := range entries {
			if info, err := e.Info(); err == nil {
				out = append(out, info)
			}
		}
		return listerat(out), nil
	case "Stat", "Lstat":
		info, err := os.Lstat(full)
		if err != nil {
			return nil, sftp.ErrSSHFxNoSuchFile
		}
		return listerat{info}, nil
	}
	return nil, sftp.ErrSSHFxOpUnsupported
}
