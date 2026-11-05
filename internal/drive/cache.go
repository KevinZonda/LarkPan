package drive

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ErrNotFound is returned when the remote path does not resolve to a file.
var ErrNotFound = fmt.Errorf("not found")

// ErrIsDir is returned when the remote path resolves to a folder.
var ErrIsDir = fmt.Errorf("is a folder")

// Cache is a read-through local-disk cache for remote Drive files.
//
// Remote metadata (folder listings) is cached for MetaTTL; file content is
// cached under the remote relative path, stamped with the remote
// modified_time. A hit is "local file exists and its mtime equals the
// remote modified_time" — no sidecar state needed. Remote overwrite
// bumps modified_time, which naturally invalidates the stale entry.
type Cache struct {
	client  *Client
	dir     string
	metaTTL time.Duration

	mu      sync.Mutex
	folders map[string]*folderView // folder token -> listing snapshot

	inflightMu sync.Mutex
	inflight   map[string]*dlCall // dst path -> in-progress download
}

type folderView struct {
	at      time.Time
	entries map[string]cacheEntry
}

type cacheEntry struct {
	token    string
	isDir    bool
	modified time.Time
}

type dlCall struct {
	done chan struct{}
	err  error
}

// NewCache creates a cache rooted at dir. metaTTL <= 0 means every access
// re-fetches the listing (always fresh, one API call per access).
func NewCache(c *Client, dir string, metaTTL time.Duration) (*Cache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Cache{
		client:   c,
		dir:      dir,
		metaTTL:  metaTTL,
		folders:  make(map[string]*folderView),
		inflight: make(map[string]*dlCall),
	}, nil
}

// Dir returns the local cache root.
func (c *Cache) Dir() string { return c.dir }

// Get resolves relPath inside the remote folder rooted at folderToken and
// returns a local path whose contents match the remote file. Concurrent
// misses on the same file are single-flighted into one download.
func (c *Cache) Get(ctx context.Context, folderToken, relPath string) (string, error) {
	ent, err := c.resolve(ctx, folderToken, relPath)
	if err != nil {
		return "", err
	}
	dst := filepath.Join(c.dir, filepath.FromSlash(relPath))
	if st, err := os.Stat(dst); err == nil && !st.IsDir() && st.ModTime().Equal(ent.modified) {
		return dst, nil // cache hit
	}
	return c.fetch(ctx, ent, dst)
}

// Stat resolves relPath to a file and returns its metadata without
// downloading content.
func (c *Cache) Stat(ctx context.Context, folderToken, relPath string) (DirEntry, error) {
	ent, err := c.resolve(ctx, folderToken, relPath)
	if err != nil {
		return DirEntry{}, err
	}
	return DirEntry{
		Name:     filepath.Base(filepath.FromSlash(relPath)),
		Token:    ent.token,
		Modified: ent.modified,
	}, nil
}

// DirEntry describes one child of a remote folder.
type DirEntry struct {
	Name     string
	IsDir    bool
	Token    string
	Modified time.Time
	Size     int64 // local cache size when already fetched; may be 0 (the list API does not expose remote size)
}

// ReadDir lists the children of the remote folder at relPath relative to
// folderToken ("" = the root folder itself).
func (c *Cache) ReadDir(ctx context.Context, folderToken, relPath string) ([]DirEntry, error) {
	token, err := c.resolveFolder(ctx, folderToken, relPath)
	if err != nil {
		return nil, err
	}
	view, err := c.folder(ctx, token)
	if err != nil {
		return nil, err
	}
	out := make([]DirEntry, 0, len(view.entries))
	for name, e := range view.entries {
		de := DirEntry{Name: name, IsDir: e.isDir, Token: e.token, Modified: e.modified}
		if !e.isDir {
			if st, err := os.Stat(filepath.Join(c.dir, filepath.FromSlash(joinRel(relPath, name)))); err == nil && st.ModTime().Equal(e.modified) {
				de.Size = st.Size()
			}
		}
		out = append(out, de)
	}
	return out, nil
}

// resolveFolder resolves relPath to a folder token. "" returns folderToken.
func (c *Cache) resolveFolder(ctx context.Context, folderToken, relPath string) (string, error) {
	cur := folderToken
	for _, seg := range strings.Split(strings.Trim(relPath, "/"), "/") {
		if seg == "" {
			continue
		}
		view, err := c.folder(ctx, cur)
		if err != nil {
			return "", err
		}
		e, ok := view.entries[seg]
		if !ok {
			return "", fmt.Errorf("%w: %s", ErrNotFound, relPath)
		}
		if !e.isDir {
			return "", fmt.Errorf("%w: %s is not a folder", ErrNotFound, joinRel("", seg))
		}
		cur = e.token
	}
	return cur, nil
}

// resolve walks relPath segment by segment through cached listings and
// returns the file entry. Intermediate segments must be folders.
func (c *Cache) resolve(ctx context.Context, folderToken, relPath string) (cacheEntry, error) {
	segs := strings.Split(strings.Trim(relPath, "/"), "/")
	cur := folderToken
	var ent cacheEntry
	for i, seg := range segs {
		view, err := c.folder(ctx, cur)
		if err != nil {
			return cacheEntry{}, err
		}
		e, ok := view.entries[seg]
		if !ok {
			return cacheEntry{}, fmt.Errorf("%w: %s", ErrNotFound, relPath)
		}
		if i == len(segs)-1 {
			if e.isDir {
				return cacheEntry{}, fmt.Errorf("%w: %s", ErrIsDir, relPath)
			}
			ent = e
			break
		}
		if !e.isDir {
			return cacheEntry{}, fmt.Errorf("%w: %s is not a folder", ErrNotFound, joinRel("", seg))
		}
		cur = e.token
	}
	return ent, nil
}

// folder returns a fresh-enough listing for token, refreshing on expiry.
// The mutex serializes refreshes so a cold cache cannot stampede the API.
func (c *Cache) folder(ctx context.Context, token string) (*folderView, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok := c.folders[token]; ok && time.Since(v.at) < c.metaTTL {
		return v, nil
	}
	files, err := c.client.ListFolder(ctx, token)
	if err != nil {
		if v, ok := c.folders[token]; ok {
			return v, nil // serve stale on refresh failure
		}
		return nil, err
	}
	v := &folderView{at: time.Now(), entries: make(map[string]cacheEntry, len(files))}
	for _, f := range files {
		v.entries[valStr(f.Name)] = cacheEntry{
			token:    valStr(f.Token),
			isDir:    valStr(f.Type) == "folder",
			modified: parseLarkTime(valStr(f.ModifiedTime)),
		}
	}
	c.folders[token] = v
	return v, nil
}

// fetch single-flights the download to dst.
func (c *Cache) fetch(ctx context.Context, ent cacheEntry, dst string) (string, error) {
	c.inflightMu.Lock()
	if call, ok := c.inflight[dst]; ok {
		c.inflightMu.Unlock()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-call.done:
			return dst, call.err
		}
	}
	call := &dlCall{done: make(chan struct{})}
	c.inflight[dst] = call
	c.inflightMu.Unlock()

	call.err = c.client.downloadOne(ctx, ent.token, dst)
	if call.err == nil {
		_ = os.Chtimes(dst, ent.modified, ent.modified)
	}

	c.inflightMu.Lock()
	delete(c.inflight, dst)
	c.inflightMu.Unlock()
	close(call.done)
	return dst, call.err
}
