package server

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

func TestArchiveRoundTrip(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "world", "region"), 0o755)
	os.WriteFile(filepath.Join(root, "world", "level.dat"), []byte("level"), 0o644)
	os.WriteFile(filepath.Join(root, "world", "region", "r.0.0.mca"), []byte("region"), 0o644)
	os.WriteFile(filepath.Join(root, "secret.txt"), []byte("no"), 0o644)

	dst := filepath.Join(root, "backup.tar.gz")
	if _, err := CreateArchive(dst, root, []string{"world", "secret.txt"}, func(rel string) bool { return rel == "secret.txt" }); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "restore")
	os.MkdirAll(out, 0o755)
	if err := ExtractArchive(dst, root, out, -1); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(out, "world", "region", "r.0.0.mca")); string(b) != "region" {
		t.Fatalf("unexpected content %q", b)
	}
	if _, err := os.Stat(filepath.Join(out, "secret.txt")); err == nil {
		t.Fatal("denied file was archived")
	}
}

func TestZipSlipIsRejected(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "evil.zip")
	f, _ := os.Create(src)
	zw := zip.NewWriter(f)
	w, _ := zw.Create("../../escaped.txt")
	w.Write([]byte("x"))
	zw.Close()
	f.Close()
	if err := ExtractArchive(src, root, root, -1); err == nil {
		t.Fatal("expected zip slip to be rejected")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(root)), "escaped.txt")); err == nil {
		t.Fatal("file escaped the root")
	}
}

func TestExtractRespectsBudget(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "big.bin"), make([]byte, 4096), 0o644)
	dst := filepath.Join(root, "a.tar.gz")
	if _, err := CreateArchive(dst, root, []string{"big.bin"}, nil); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "out")
	os.MkdirAll(out, 0o755)
	if err := ExtractArchive(dst, root, out, 1000); err != ErrNotEnoughDisk {
		t.Fatalf("expected ErrNotEnoughDisk, got %v", err)
	}
}

func TestIgnoreMatcher(t *testing.T) {
	skip := ignoreMatcher("logs/\n*.log\ncache\n!important.log")
	cases := map[string]bool{"logs/latest.log": true, "logs": true, "server.log": true, "important.log": false, "cache/x": true, "world/level.dat": false}
	for p, want := range cases {
		if got := skip(p); got != want {
			t.Errorf("%s: got %v want %v", p, got, want)
		}
	}
}
