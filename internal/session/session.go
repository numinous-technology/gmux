// Package session lets a GPU-less client run a command on this GPU host. The
// client syncs a workspace here (content-addressed, so only changed files
// upload), the command runs in that workspace under a real gmux share, and the
// client pulls results back. The network boundary sits at the command, not the
// CUDA call, so it is latency tolerant and needs no driver interception. This
// mirrors the numinous-gpu ngpu-server protocol.
package session

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// File is one entry in a workspace manifest.
type File struct {
	Path string `json:"path"`
	SHA  string `json:"sha"`
	Size int64  `json:"size"`
	X    bool   `json:"x,omitempty"` // executable bit
}

// Store holds content-addressed blobs shared across sessions, and each
// session's materialised workspace.
//
//	<root>/blobs/<sha>          content-addressed, shared
//	<root>/sessions/<id>/ws/    a session's working tree
type Store struct {
	root string
	mu   sync.Mutex
	seen map[string]time.Time // session id -> last touch
}

// Open creates a store under root.
func Open(root string) (*Store, error) {
	for _, d := range []string{"blobs", "sessions"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return nil, err
		}
	}
	return &Store{root: root, seen: map[string]time.Time{}}, nil
}

func (s *Store) blobPath(sha string) string { return filepath.Join(s.root, "blobs", sha) }

// Workspace returns the working directory for a session, creating the session
// if asked.
func (s *Store) Workspace(id string, create bool) (string, error) {
	if err := safeID(id); err != nil {
		return "", err
	}
	ws := filepath.Join(s.root, "sessions", id, "ws")
	if create {
		if err := os.MkdirAll(ws, 0o755); err != nil {
			return "", err
		}
		s.mu.Lock()
		s.seen[id] = time.Now()
		s.mu.Unlock()
	}
	return ws, nil
}

// HasBlob reports whether a verified blob is present.
func (s *Store) HasBlob(sha string) bool {
	if !validSHA(sha) {
		return false
	}
	fi, err := os.Stat(s.blobPath(sha))
	return err == nil && fi.Mode().IsRegular()
}

// Missing returns the subset of manifest shas not already stored.
func (s *Store) Missing(files []File) []string {
	want := map[string]bool{}
	for _, f := range files {
		if validSHA(f.SHA) {
			want[f.SHA] = true
		}
	}
	var miss []string
	for sha := range want {
		if !s.HasBlob(sha) {
			miss = append(miss, sha)
		}
	}
	sort.Strings(miss)
	return miss
}

// PutBlob stores raw bytes after verifying they hash to sha.
func (s *Store) PutBlob(sha string, data []byte) error {
	if !validSHA(sha) {
		return fmt.Errorf("bad sha")
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != sha {
		return fmt.Errorf("blob does not match its sha")
	}
	dst := s.blobPath(sha)
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// PutBlobFrom streams a blob into the store, verifying it hashes to sha before
// it becomes visible. A large upload is never held in memory.
func (s *Store) PutBlobFrom(sha string, r io.Reader) (int64, error) {
	if !validSHA(sha) {
		return 0, fmt.Errorf("bad sha")
	}
	dst := s.blobPath(sha)
	f, err := os.CreateTemp(filepath.Dir(dst), ".upload-*")
	if err != nil {
		return 0, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return n, err
	}
	if hex.EncodeToString(h.Sum(nil)) != sha {
		os.Remove(f.Name())
		return n, fmt.Errorf("blob does not match its sha")
	}
	return n, os.Rename(f.Name(), dst)
}

// Apply materialises the manifest into the session workspace. With prune it
// removes files not in the manifest, so the workspace exactly matches the
// client.
func (s *Store) Apply(id string, files []File, prune bool) error {
	ws, err := s.Workspace(id, true)
	if err != nil {
		return err
	}
	wanted := map[string]bool{}
	for _, f := range files {
		rel, err := safeRel(f.Path)
		if err != nil {
			return err
		}
		if !s.HasBlob(f.SHA) {
			return fmt.Errorf("missing blob for %s (upload it first)", rel)
		}
		wanted[rel] = true
		dst := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		data, err := os.ReadFile(s.blobPath(f.SHA))
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if f.X {
			mode = 0o755
		}
		if err := os.WriteFile(dst, data, mode); err != nil {
			return err
		}
	}
	if prune {
		filepath.Walk(ws, func(p string, fi os.FileInfo, err error) error {
			if err != nil || fi.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(ws, p)
			if !wanted[filepath.ToSlash(rel)] {
				os.Remove(p)
			}
			return nil
		})
	}
	s.mu.Lock()
	s.seen[id] = time.Now()
	s.mu.Unlock()
	return nil
}

// Fetch returns a gzip tarball of the workspace files matching the globs.
func (s *Store) Fetch(id string, globs []string) ([]byte, error) {
	ws, err := s.Workspace(id, false)
	if err != nil {
		return nil, err
	}
	matches := map[string]bool{}
	for _, g := range globs {
		if strings.Contains(g, "..") || strings.HasPrefix(g, "/") {
			return nil, fmt.Errorf("unsafe glob %q", g)
		}
		hits, _ := filepath.Glob(filepath.Join(ws, filepath.FromSlash(g)))
		for _, m := range hits {
			if fi, err := os.Stat(m); err == nil && fi.Mode().IsRegular() {
				matches[m] = true
			}
		}
	}
	var paths []string
	for p := range matches {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var buf strings.Builder
	gz := gzip.NewWriter(&writerAdapter{&buf})
	tw := tar.NewWriter(gz)
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		rel, _ := filepath.Rel(ws, p)
		hdr := &tar.Header{Name: filepath.ToSlash(rel), Mode: 0o644, Size: int64(len(data))}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, err
		}
	}
	tw.Close()
	gz.Close()
	return []byte(buf.String()), nil
}

// Delete removes a session's workspace. Blobs are shared and left alone.
func (s *Store) Delete(id string) error {
	if err := safeID(id); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.seen, id)
	s.mu.Unlock()
	return os.RemoveAll(filepath.Join(s.root, "sessions", id))
}

type writerAdapter struct{ b *strings.Builder }

func (w *writerAdapter) Write(p []byte) (int, error) { w.b.Write(p); return len(p), nil }

func validSHA(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func safeID(id string) error {
	if id == "" || strings.ContainsAny(id, "/\\.") {
		return fmt.Errorf("bad session id %q", id)
	}
	return nil
}

func safeRel(rel string) (string, error) {
	rel = filepath.ToSlash(rel)
	if rel == "" || strings.HasPrefix(rel, "/") || strings.Contains(rel, "..") {
		return "", fmt.Errorf("unsafe path %q", rel)
	}
	return rel, nil
}
