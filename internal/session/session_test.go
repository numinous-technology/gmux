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
