package agent

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/PaulOh5/gpu-private-cloud-with-gstack/internal/workdir"
)

func TestWorkdirStagerReceiveAndGate(t *testing.T) {
	st, err := NewWorkdirStager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// Not ready before any upload.
	if _, ready := st.Dir("job1"); ready {
		t.Fatal("Dir should not be ready before upload")
	}

	// Build a tar of a small source dir.
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "train.py"), []byte("print(1)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var tarBuf bytes.Buffer
	if err := workdir.Tar(src, &tarBuf); err != nil {
		t.Fatal(err)
	}

	// Upload it through the handler.
	srv := httptest.NewServer(st.Handler())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/workdir/job1", "application/octet-stream", &tarBuf)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("upload status = %d, want 204", resp.StatusCode)
	}

	// Now ready, and the file is present in the staged dir.
	dir, ready := st.Dir("job1")
	if !ready {
		t.Fatal("Dir should be ready after upload")
	}
	if b, err := os.ReadFile(filepath.Join(dir, "train.py")); err != nil || string(b) != "print(1)\n" {
		t.Fatalf("staged train.py = %q err=%v", b, err)
	}

	// Cleanup removes the dir and clears readiness.
	st.Cleanup("job1")
	if _, ready := st.Dir("job1"); ready {
		t.Fatal("Dir should not be ready after cleanup")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("staged dir should be removed on cleanup")
	}
}
