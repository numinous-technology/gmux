package remote

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// hashCache remembers each file's sha256 by its identity (device, inode, size,
// modification time), so syncing a directory with large unchanged files, like
// model weights, does not read them again. It lives in the user's cache
// directory, or $GMUX_HASH_CACHE ("off" disables it).
type hashCache struct {
	path  string
	mu    sync.Mutex
	m     map[string]cacheEntry
	dirty bool
}

type cacheEntry struct {
	Dev, Ino    uint64
	Size, Mtime int64
	SHA         string
}

func openHashCache() *hashCache {
	p := os.Getenv("GMUX_HASH_CACHE")
	if p == "off" {
		return nil
	}
	if p == "" {
		dir, err := os.UserCacheDir()
		if err != nil {
			return nil
		}
		p = filepath.Join(dir, "gmux", "hashes.json")
	}
	h := &hashCache{path: p, m: map[string]cacheEntry{}}
	if b, err := os.ReadFile(p); err == nil {
		json.Unmarshal(b, &h.m)
	}
	return h
}

func identity(fi os.FileInfo) (dev, ino uint64) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Dev), st.Ino
	}
	return 0, 0
}

// hashFile returns the file's sha256, from the cache when its identity is
// unchanged, otherwise by streaming it. hashed reports whether it was read.
func (h *hashCache) hashFile(path string, fi os.FileInfo) (sum string, hashed bool, err error) {
	dev, ino := identity(fi)
	if h != nil {
		h.mu.Lock()
		e, ok := h.m[path]
		h.mu.Unlock()
		if ok && e.Dev == dev && e.Ino == ino && e.Size == fi.Size() && e.Mtime == fi.ModTime().UnixNano() {
			return e.SHA, false, nil
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	hs := sha256.New()
	if _, err := io.Copy(hs, f); err != nil {
		return "", false, err
	}
	sum = hex.EncodeToString(hs.Sum(nil))
	if h != nil {
		h.mu.Lock()
		h.m[path] = cacheEntry{dev, ino, fi.Size(), fi.ModTime().UnixNano(), sum}
		h.dirty = true
		h.mu.Unlock()
	}
	return sum, true, nil
}

func (h *hashCache) save() {
	if h == nil || !h.dirty {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.m) > 200000 { // bound it: start over rather than grow forever
		h.m = map[string]cacheEntry{}
	}
	os.MkdirAll(filepath.Dir(h.path), 0o700)
	b, _ := json.Marshal(h.m)
	tmp := h.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, h.path)
	}
	h.dirty = false
}
