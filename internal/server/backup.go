package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/ammonode/daemon/internal/config"
)

// BackupPath is where a local backup archive is stored.
func BackupPath(uuid string) string {
	return filepath.Join(config.Get().System.BackupDir, filepath.Base(uuid)+".tar.gz")
}

// ignoreMatcher implements a small subset of .gitignore: globs, "dir/" and "!negation".
func ignoreMatcher(patterns string) func(rel string) bool {
	var rules []string
	for _, l := range strings.Split(patterns, "\n") {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasPrefix(l, "#") {
			rules = append(rules, l)
		}
	}
	return func(rel string) bool {
		rel = filepath.ToSlash(rel)
		ignored := false
		for _, r := range rules {
			neg := strings.HasPrefix(r, "!")
			p := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(r, "!"), "/"), "/")
			match := false
			if ok, _ := path.Match(p, rel); ok {
				match = true
			} else if ok, _ := path.Match(p, path.Base(rel)); ok && !strings.Contains(p, "/") {
				match = true
			} else if strings.HasPrefix(rel, p+"/") {
				match = true
			}
			if match {
				ignored = !neg
			}
		}
		return ignored
	}
}

func sha256File(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Backup archives the whole server directory and reports the result to the panel.
// With uploadURL (a presigned S3 PUT) the archive is uploaded and the local copy removed.
func (s *Server) Backup(backupUUID, ignore, uploadURL string) {
	start := time.Now()
	dst := BackupPath(backupUUID)
	_ = os.MkdirAll(filepath.Dir(dst), 0o700)

	// A .ammonodeignore file in the server root adds more patterns.
	if b, err := os.ReadFile(filepath.Join(s.Dir(), ".ammonodeignore")); err == nil {
		ignore += "\n" + string(b)
	}
	skip := ignoreMatcher(ignore)
	entries, err := os.ReadDir(s.Dir())
	var size int64
	var checksum string
	if err == nil {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		size, err = CreateArchive(dst, s.Dir(), names, skip)
	}
	if err == nil {
		checksum, err = sha256File(dst)
	}
	if err == nil && uploadURL != "" {
		err = uploadFile(dst, uploadURL)
		os.Remove(dst)
	}
	if err != nil {
		os.Remove(dst)
		s.log.Error("backup failed", "backup", backupUUID, "error", err)
	} else {
		s.log.Info("backup completed", "backup", backupUUID, "size", size, "took", time.Since(start).Round(time.Millisecond))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	report := map[string]interface{}{"successful": err == nil, "checksum": checksum, "checksum_type": "sha256", "size": size}
	if rerr := s.panel.SetBackupStatus(ctx, backupUUID, report); rerr != nil {
		s.log.Error("could not report backup status", "error", rerr)
	}
	s.Events.Publish("backup completed", backupUUID)
}

// Restore stops the server and extracts a backup over its files. With truncate
// every existing file is deleted first.
func (s *Server) Restore(backupUUID string, truncate bool, downloadURL string) error {
	src := BackupPath(backupUUID)
	if downloadURL != "" {
		// S3 backup: download to the local backup folder first, remove it afterwards.
		if err := downloadFile(downloadURL, src); err != nil {
			s.daemonMessage("Could not download the backup: " + err.Error())
			return err
		}
		defer os.Remove(src)
	}
	if _, err := os.Stat(src); err != nil {
		return errors.New("backup file not found on this node")
	}
	if err := s.WaitForStop(context.Background(), time.Minute, true); err != nil {
		return err
	}
	s.daemonMessage("Restoring backup, the server cannot be started until it finishes.")
	if truncate {
		entries, _ := os.ReadDir(s.Dir())
		for _, e := range entries {
			_ = os.RemoveAll(filepath.Join(s.Dir(), e.Name()))
		}
		s.UpdateDiskUsage()
	}
	err := ExtractArchive(src, s.Dir(), s.Dir(), s.RemainingSpace())
	cfg := config.Get()
	_ = filepath.Walk(s.Dir(), func(p string, _ os.FileInfo, err error) error {
		if err == nil {
			_ = os.Lchown(p, cfg.System.User.UID, cfg.System.User.GID)
		}
		return nil
	})
	s.UpdateDiskUsage()
	if err != nil {
		s.daemonMessage("Backup restore failed: " + err.Error())
		return err
	}
	s.daemonMessage("Backup restored.")
	return nil
}

var transferClient = &http.Client{Timeout: 6 * time.Hour}

func uploadFile(path, url string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPut, url, f)
	if err != nil {
		return err
	}
	req.ContentLength = st.Size()
	req.Header.Set("Content-Type", "application/gzip")
	res, err := transferClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("upload failed: %s %s", res.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

func downloadFile(url, dst string) error {
	res, err := transferClient.Get(url)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		return fmt.Errorf("download failed: %s", res.Status)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, res.Body); err != nil {
		f.Close()
		os.Remove(dst)
		return err
	}
	return f.Close()
}
