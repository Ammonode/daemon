package server

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ErrUnsupportedArchive is returned for files we cannot extract.
var ErrUnsupportedArchive = errors.New("unsupported archive format (use .zip, .tar, .tar.gz or .tgz)")

// CreateArchive writes a .tar.gz containing the given paths (relative to base).
// Files matching the egg deny list are skipped.
func CreateArchive(dst, base string, paths []string, skip func(rel string) bool) (int64, error) {
	out, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)

	for _, p := range paths {
		root := filepath.Join(base, p)
		err := filepath.Walk(root, func(path string, info fs.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(base, path)
			if skip != nil && skip(rel) {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			// Never follow symlinks out of the server directory.
			if info.Mode()&os.ModeSymlink != 0 {
				return nil
			}
			hdr, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			hdr.Name = filepath.ToSlash(rel)
			if info.IsDir() {
				hdr.Name += "/"
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			if info.Mode().IsRegular() {
				f, err := os.Open(path)
				if err != nil {
					return err
				}
				_, err = io.Copy(tw, f)
				f.Close()
				return err
			}
			return nil
		})
		if err != nil {
			return 0, err
		}
	}
	if err := tw.Close(); err != nil {
		return 0, err
	}
	if err := gz.Close(); err != nil {
		return 0, err
	}
	st, err := out.Stat()
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// ExtractArchive extracts src into dir. Every entry is checked with SafePath
// (no zip-slip) and the running total is checked against the space budget.
func ExtractArchive(src, root, dir string, budget int64) error {
	name := strings.ToLower(src)
	switch {
	case strings.HasSuffix(name, ".zip"):
		return extractZip(src, root, dir, budget)
	case strings.HasSuffix(name, ".tar.gz"), strings.HasSuffix(name, ".tgz"):
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		return extractTar(gz, root, dir, budget)
	case strings.HasSuffix(name, ".tar"):
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()
		return extractTar(f, root, dir, budget)
	}
	return ErrUnsupportedArchive
}

// escapes reports whether a cleaned relative path points outside its root.
func escapes(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

type budgetWriter struct {
	w      io.Writer
	budget *int64
}

func (b budgetWriter) Write(p []byte) (int, error) {
	if *b.budget >= 0 {
		*b.budget -= int64(len(p))
		if *b.budget < 0 {
			return 0, ErrNotEnoughDisk
		}
	}
	return b.w.Write(p)
}

func writeEntry(target string, mode os.FileMode, r io.Reader, budget *int64) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode.Perm()|0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(budgetWriter{f, budget}, r)
	return err
}

func extractTar(r io.Reader, root, dir string, budget int64) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, filepath.Join(dir, hdr.Name))
		if err != nil || escapes(rel) {
			return fmt.Errorf("%s: %w", hdr.Name, ErrBadPath)
		}
		target, err := SafePath(root, rel)
		if err != nil {
			return fmt.Errorf("%s: %w", hdr.Name, err)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := writeEntry(target, os.FileMode(hdr.Mode), tr, &budget); err != nil {
				return err
			}
		}
		// Symlinks, devices and hard links are skipped on purpose.
	}
}

func extractZip(src, root, dir string, budget int64) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		rel, err := filepath.Rel(root, filepath.Join(dir, f.Name))
		if err != nil || escapes(rel) {
			return fmt.Errorf("%s: %w", f.Name, ErrBadPath)
		}
		target, err := SafePath(root, rel)
		if err != nil {
			return fmt.Errorf("%s: %w", f.Name, err)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if !f.Mode().IsRegular() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		err = writeEntry(target, f.Mode(), rc, &budget)
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// RemainingSpace returns the bytes still available under the disk limit (-1 = unlimited).
func (s *Server) RemainingSpace() int64 {
	limit := s.Config().Build.DiskSpace
	if limit <= 0 {
		return -1
	}
	left := limit*1024*1024 - s.DiskUsage()
	if left < 0 {
		return 0
	}
	return left
}
