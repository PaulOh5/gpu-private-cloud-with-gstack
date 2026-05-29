package workdir

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestTarUntarRoundTrip(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "train.py"), "print('hi')\n")
	writeFile(t, filepath.Join(src, "pkg", "mod.py"), "x = 1\n")
	// A skipped dir must not travel.
	writeFile(t, filepath.Join(src, ".git", "config"), "secret\n")
	writeFile(t, filepath.Join(src, "node_modules", "big.js"), "//big\n")

	var buf bytes.Buffer
	if err := Tar(src, &buf); err != nil {
		t.Fatalf("Tar: %v", err)
	}

	dst := t.TempDir()
	if err := Untar(&buf, dst); err != nil {
		t.Fatalf("Untar: %v", err)
	}

	if got := readFile(t, filepath.Join(dst, "train.py")); got != "print('hi')\n" {
		t.Errorf("train.py = %q", got)
	}
	if got := readFile(t, filepath.Join(dst, "pkg", "mod.py")); got != "x = 1\n" {
		t.Errorf("pkg/mod.py = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dst, ".git")); !os.IsNotExist(err) {
		t.Error(".git should have been skipped")
	}
	if _, err := os.Stat(filepath.Join(dst, "node_modules")); !os.IsNotExist(err) {
		t.Error("node_modules should have been skipped")
	}
}

// TestUntarRejectsZipSlip builds a malicious archive with a ../ entry and
// confirms Untar refuses to write outside destDir.
func TestUntarRejectsZipSlip(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("pwned")
	tw.WriteHeader(&tar.Header{Name: "../escape.txt", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
	tw.Write(body)
	tw.Close()
	gz.Close()

	dst := t.TempDir()
	if err := Untar(&buf, dst); err == nil {
		t.Fatal("Untar should reject ../ escape")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dst), "escape.txt")); !os.IsNotExist(err) {
		t.Fatal("zip-slip wrote a file outside destDir")
	}
}

func TestUntarRejectsAbsolutePath(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "/etc/evil", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
	tw.Write([]byte("x"))
	tw.Close()
	gz.Close()
	if err := Untar(&buf, t.TempDir()); err == nil {
		t.Fatal("Untar should reject absolute paths")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
