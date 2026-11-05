package server

// Read-only WebDAV adapter over the drive cache, so Windows can mount
// the tree as a network drive (net use) with no extra software.

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"

	"golang.org/x/net/webdav"

	"larkpan/internal/drive"
)

type davFS struct {
	cache *drive.Cache
	root  string
}

func newDAVFS(cache *drive.Cache, root string) *davFS {
	return &davFS{cache: cache, root: root}
}

var errRO = errors.New("read-only filesystem")

func (d *davFS) Mkdir(context.Context, string, os.FileMode) error { return errRO }
func (d *davFS) RemoveAll(context.Context, string) error          { return errRO }
func (d *davFS) Rename(context.Context, string, string) error     { return errRO }

func (d *davFS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_APPEND|os.O_TRUNC) != 0 {
		return nil, errRO
	}
	rel := strings.TrimPrefix(name, "/")
	if rel == "" {
		return &davDir{fsys: d, rel: ""}, nil
	}
	local, err := d.cache.Get(ctx, d.root, rel)
	if errors.Is(err, drive.ErrIsDir) {
		return &davDir{fsys: d, rel: rel}, nil
	}
	if errors.Is(err, drive.ErrNotFound) {
		return nil, fs.ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	f, err := os.Open(local)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fs.ErrNotExist
	}
	return &davFile{File: f}, err
}

func (d *davFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	rel := strings.TrimPrefix(name, "/")
	if rel == "" {
		return dirInfo{time.Now()}, nil
	}
	e, err := d.cache.Stat(ctx, d.root, rel)
	if errors.Is(err, drive.ErrIsDir) {
		return dirInfo{time.Now()}, nil
	}
	if err != nil {
		// Not a file — maybe a folder (trailing-slash paths etc).
		if _, lerr := d.cache.ReadDir(ctx, d.root, rel); lerr == nil {
			return dirInfo{time.Now()}, nil
		}
		return nil, fs.ErrNotExist
	}
	fi := fileInfo{name: path.Base(rel), mod: e.Modified}
	// Real size only when already cached locally.
	if p, gerr := d.cache.Get(ctx, d.root, rel); gerr == nil {
		if st, serr := os.Stat(p); serr == nil {
			fi.size = st.Size()
		}
	}
	return fi, nil
}

// davFile is a read file; writes are denied.
type davFile struct{ *os.File }

func (davFile) Write([]byte) (int, error) { return 0, errRO }

// davDir synthesizes a directory File from the remote listing.
type davDir struct {
	fsys   *davFS
	rel    string
	ents   []drive.DirEntry
	idx    int
	loaded bool
}

func (d *davDir) Close() error                   { return nil }
func (d *davDir) Write([]byte) (int, error)      { return 0, errRO }
func (d *davDir) Read([]byte) (int, error)       { return 0, errors.New("is a directory") }
func (d *davDir) Seek(int64, int) (int64, error) { return 0, errors.New("is a directory") }

func (d *davDir) Stat() (os.FileInfo, error) { return dirInfo{time.Now()}, nil }

func (d *davDir) Readdir(count int) ([]os.FileInfo, error) {
	if !d.loaded {
		ents, err := d.fsys.cache.ReadDir(context.Background(), d.fsys.root, d.rel)
		if err != nil {
			return nil, err
		}
		d.ents = ents
		d.loaded = true
	}
	if d.idx >= len(d.ents) && count > 0 {
		return nil, io.EOF
	}
	remain := d.ents[d.idx:]
	if count > 0 && len(remain) > count {
		remain = remain[:count]
	}
	out := make([]os.FileInfo, 0, len(remain))
	for _, e := range remain {
		var fi os.FileInfo
		if e.IsDir {
			fi = dirInfo{e.Modified}
		} else {
			fi = fileInfo{name: e.Name, size: e.Size, mod: e.Modified}
		}
		// FileInfo.Name must be the base name.
		out = append(out, named{fi, e.Name})
	}
	d.idx += len(remain)
	return out, nil
}

type named struct {
	os.FileInfo
	name string
}

func (n named) Name() string { return n.name }

type fileInfo struct {
	name string
	size int64
	mod  time.Time
}

func (f fileInfo) Name() string       { return f.name }
func (f fileInfo) Size() int64        { return f.size }
func (f fileInfo) Mode() os.FileMode  { return 0o444 }
func (f fileInfo) ModTime() time.Time { return f.mod }
func (f fileInfo) IsDir() bool        { return false }
func (f fileInfo) Sys() any           { return nil }

type dirInfo struct{ mod time.Time }

func (d dirInfo) Name() string       { return "." }
func (d dirInfo) Size() int64        { return 0 }
func (d dirInfo) Mode() os.FileMode  { return fs.ModeDir | 0o555 }
func (d dirInfo) ModTime() time.Time { return d.mod }
func (d dirInfo) IsDir() bool        { return true }
func (d dirInfo) Sys() any           { return nil }
