package session

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sha(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func TestManifestApplyPruneAndFetch(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, b := []byte("alpha"), []byte("beta")
	files := []File{{Path: "a.txt", SHA: sha(a), Size: 5}, {Path: "sub/b.txt", SHA: sha(b), Size: 4, X: true}}

	miss := st.Missing(files)
	if len(miss) != 2 {
		t.Fatalf("both blobs should be missing first: %v", miss)
	}
	if err := st.PutBlob(sha(a), a); err != nil {
		t.Fatal(err)
	}
	if got := st.Missing(files); len(got) != 1 || got[0] != sha(b) {
		t.Fatalf("only b should remain missing: %v", got)
	}
	if err := st.PutBlob(sha(b), b); err != nil {
		t.Fatal(err)
	}
	if err := st.Apply("sess1", files, true); err != nil {
		t.Fatal(err)
	}
	ws, _ := st.Workspace("sess1", false)
	if body, _ := os.ReadFile(filepath.Join(ws, "sub/b.txt")); string(body) != "beta" {
		t.Fatalf("apply did not materialise: %q", body)
	}
	if fi, _ := os.Stat(filepath.Join(ws, "sub/b.txt")); fi.Mode()&0o100 == 0 {
		t.Fatal("executable bit should be preserved")
	}

	// prune: a second apply without a.txt should remove it
	if err := st.Apply("sess1", files[1:], true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws, "a.txt")); !os.IsNotExist(err) {
		t.Fatal("prune should have removed a.txt")
	}

	// fetch
	tgz, err := st.Fetch("sess1", []string{"sub/*.txt"})
	if err != nil {
		t.Fatal(err)
	}
	names := tarNames(t, tgz)
	if len(names) != 1 || names[0] != "sub/b.txt" {
		t.Fatalf("fetch returned wrong set: %v", names)
	}
}

func TestPutBlobRejectsMismatch(t *testing.T) {
	st, _ := Open(t.TempDir())
	if err := st.PutBlob(sha([]byte("x")), []byte("y")); err == nil {
		t.Fatal("a blob whose bytes do not match its sha must be refused")
	}
}

func TestRejectsPathTraversal(t *testing.T) {
	st, _ := Open(t.TempDir())
	st.PutBlob(sha([]byte("z")), []byte("z"))
	if err := st.Apply("s", []File{{Path: "../escape", SHA: sha([]byte("z"))}}, false); err == nil {
		t.Fatal("path traversal must be rejected")
	}
	if _, err := st.Workspace("../evil", true); err == nil {
		t.Fatal("bad session id must be rejected")
	}
}

func tarNames(t *testing.T, tgz []byte) []string {
	t.Helper()
	gz, err := gzip.NewReader(strings.NewReader(string(tgz)))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
	return names
}

func TestPutBlobFromStreamsAndVerifies(t *testing.T) {
	st, _ := Open(t.TempDir())
	data := []byte(strings.Repeat("model weights ", 100000))
	n, err := st.PutBlobFrom(sha(data), strings.NewReader(string(data)))
	if err != nil || n != int64(len(data)) || !st.HasBlob(sha(data)) {
		t.Fatalf("stream put: n=%d err=%v", n, err)
	}
	if _, err := st.PutBlobFrom(sha([]byte("x")), strings.NewReader("y")); err == nil {
		t.Fatal("a streamed blob that does not match its sha must be refused")
	}
	if st.HasBlob(sha([]byte("x"))) {
		t.Fatal("a refused upload must not become visible")
	}
}

func TestApplySkipsUnchangedFilesButRestoresOnesTheJobChanged(t *testing.T) {
	st, _ := Open(t.TempDir())
	body := []byte("weights")
	st.PutBlob(sha(body), body)
	files := []File{{Path: "w.bin", SHA: sha(body), Size: int64(len(body))}}
	if err := st.Apply("s", files, true); err != nil {
		t.Fatal(err)
	}
	ws, _ := st.Workspace("s", false)
	p := filepath.Join(ws, "w.bin")
	old := time.Now().Add(-time.Hour)
	os.Chtimes(p, old, old)
	// the record now disagrees with the file's mtime, as if the job touched it:
	// it must be rewritten
	st.Apply("s", files, true)
	fi1, _ := os.Stat(p)
	if fi1.ModTime().Equal(old) {
		t.Fatal("a file whose mtime changed since the last apply must be rewritten")
	}
	// unchanged since that apply: left alone
	st.Apply("s", files, true)
	fi2, _ := os.Stat(p)
	if !fi2.ModTime().Equal(fi1.ModTime()) {
		t.Fatal("an unchanged file must not be rewritten")
	}
	// the job overwrote it with other content of the same size
	os.WriteFile(p, []byte("changed"), 0o644)
	st.Apply("s", files, true)
	if b, _ := os.ReadFile(p); string(b) != "weights" {
		t.Fatalf("apply must restore the client's version, got %q", b)
	}
}
