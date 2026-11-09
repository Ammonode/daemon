package server

import (
	"errors"
	"io/fs"
	"net/netip"
	"path/filepath"
	"strings"
	"time"
)

var ErrNotEnoughDisk = errors.New("not enough disk space available")
var ErrBadPath = errors.New("path resolves outside of the server directory")

// SafePath joins p onto root and makes sure the result (after resolving
// symlinks of existing parents) cannot escape root.
func SafePath(root, p string) (string, error) {
	clean := filepath.Join(root, filepath.Clean("/"+p))
	if !strings.HasPrefix(clean, filepath.Clean(root)) {
		return "", ErrBadPath
	}
	// Resolve the deepest existing parent to catch symlink escapes.
	dir := clean
	for {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			rootResolved, rerr := filepath.EvalSymlinks(root)
			if rerr != nil {
				rootResolved = root
			}
			if resolved != rootResolved && !strings.HasPrefix(resolved, rootResolved+string(filepath.Separator)) {
				return "", ErrBadPath
			}
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return clean, nil
}

// UpdateDiskUsage walks the data directory and caches the size in bytes.
func (s *Server) UpdateDiskUsage() int64 {
	var size int64
	_ = filepath.WalkDir(s.Dir(), func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			if info, err := d.Info(); err == nil {
				size += info.Size()
			}
		}
		return nil
	})
	s.mu.Lock()
	s.diskUsage = size
	s.mu.Unlock()
	return size
}

func (s *Server) DiskUsage() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.diskUsage
}

// HasSpaceFor reports whether writing n more bytes stays within the disk limit.
func (s *Server) HasSpaceFor(n int64) bool {
	limit := s.Config().Build.DiskSpace
	return limit <= 0 || s.DiskUsage()+n <= limit*1024*1024
}

func (s *Server) checkDisk() error {
	s.UpdateDiskUsage()
	if !s.HasSpaceFor(0) {
		return ErrNotEnoughDisk
	}
	return nil
}

// DiskLoop refreshes disk usage periodically until stop is closed.
func (s *Server) DiskLoop(interval time.Duration, stop <-chan struct{}) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			s.UpdateDiskUsage()
		case <-stop:
			return
		}
	}
}

func parseAddrs(list []string) []netip.Addr {
	out := make([]netip.Addr, 0, len(list))
	for _, a := range list {
		if ip, err := netip.ParseAddr(a); err == nil {
			out = append(out, ip)
		}
	}
	return out
}
