//go:build linux

// Package mount exposes the larkpan edge server as a local filesystem
// via FUSE. Files are read lazily over HTTP with Range requests —
// nothing is downloaded until it is read.
package mount

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// Client talks to a larkpan serve endpoint with a bearer token.
type Client struct {
	Base  string
	Token string
	hc    *http.Client
}

// Mount attaches the remote tree at dir and blocks until unmounted.
func Mount(c *Client, dir string) error {
	c.hc = &http.Client{Timeout: 60 * time.Second}
	root := &node{cli: c, rel: "", isDir: true}
	srv, err := fuse.NewServer(fs.NewNodeFS(root, &fs.Options{}), dir, &fuse.MountOptions{})
	if err != nil {
		return err
	}
	go srv.Serve()
	if err := srv.WaitMount(); err != nil {
		return err
	}
	srv.Wait()
	return nil
}

func (c *Client) req(ctx context.Context, method, p string, hdr http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.Base+p, nil)
	if err != nil {
		return nil, err
	}
	for k, vs := range hdr {
		req.Header[k] = vs
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	return c.hc.Do(req)
}

type jsonEntry struct {
	Name     string    `json:"name"`
	Type     string    `json:"type"` // "file" | "dir"
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

// list returns directory entries at rel ("" = root).
func (c *Client) list(ctx context.Context, rel string) ([]jsonEntry, error) {
	p := "/json/"
	if rel != "" {
		p += rel + "/"
	}
	resp, err := c.req(ctx, http.MethodGet, p, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, syscall.ENOENT
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("list %s: %s: %s", rel, resp.Status, b)
	}
	var ents []jsonEntry
	return ents, json.NewDecoder(resp.Body).Decode(&ents)
}

// stat resolves rel to (isDir, size, mtime). File size/mtime come from a
// HEAD request, which also triggers server-side fetch if uncached.
func (c *Client) stat(ctx context.Context, rel string) (isDir bool, size int64, mtime time.Time, err error) {
	// File? HEAD /files/<rel>
	resp, err := c.req(ctx, http.MethodHead, "/files/"+rel, nil)
	if err != nil {
		return false, 0, mtime, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		mt, _ := http.ParseTime(resp.Header.Get("Last-Modified"))
		return false, resp.ContentLength, mt, nil
	case http.StatusNotFound:
		return false, 0, mtime, syscall.ENOENT
	}
	// Not a file: treat as dir if listing works.
	if _, lerr := c.list(ctx, rel); lerr == nil {
		return true, 0, mtime, nil
	}
	return false, 0, mtime, syscall.ENOENT
}

// readRange fetches bytes [off, off+len) of the file at rel.
func (c *Client) readRange(ctx context.Context, rel string, off, length int64) ([]byte, error) {
	hdr := http.Header{"Range": {fmt.Sprintf("bytes=%d-%d", off, off+length-1)}}
	resp, err := c.req(ctx, http.MethodGet, "/files/"+rel, hdr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("read %s: %s", rel, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, length))
}

// node is a lazily-resolved FUSE node keyed by remote relative path.
type node struct {
	fs.Inode
	cli   *Client
	rel   string
	isDir bool
	size  int64
	mtime time.Time
}

var _ fs.NodeLookuper = (*node)(nil)
var _ fs.NodeReaddirer = (*node)(nil)
var _ fs.NodeGetattrer = (*node)(nil)
var _ fs.NodeOpener = (*node)(nil)

func (n *node) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	rel := path.Join(n.rel, name)
	isDir, size, mtime, err := n.cli.stat(ctx, rel)
	if err != nil {
		return nil, toErrno(err)
	}
	child := &node{cli: n.cli, rel: rel, isDir: isDir, size: size, mtime: mtime}
	var st fs.StableAttr
	if isDir {
		st.Mode = fuse.S_IFDIR
	} else {
		st.Mode = fuse.S_IFREG
	}
	out.Attr.Mode = uint32(st.Mode) | 0o444
	if isDir {
		out.Attr.Mode = fuse.S_IFDIR | 0o555
	}
	out.Attr.Size = uint64(size)
	out.Attr.Mtime = uint64(mtime.Unix())
	return n.NewInode(ctx, child, st), 0
}

func (n *node) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	ents, err := n.cli.list(ctx, n.rel)
	if err != nil {
		return nil, toErrno(err)
	}
	out := make([]fuse.DirEntry, 0, len(ents))
	for _, e := range ents {
		mode := uint32(fuse.S_IFREG)
		if e.Type == "dir" {
			mode = fuse.S_IFDIR
		}
		out = append(out, fuse.DirEntry{Name: e.Name, Mode: mode})
	}
	return fs.NewListDirStream(out), 0
}

func (n *node) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	if n.isDir {
		out.Attr.Mode = fuse.S_IFDIR | 0o555
		return 0
	}
	out.Attr.Mode = fuse.S_IFREG | 0o444
	out.Attr.Size = uint64(n.size)
	out.Attr.Mtime = uint64(n.mtime.Unix())
	return 0
}

func (n *node) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if n.isDir {
		return nil, 0, syscall.EISDIR
	}
	return &fileHandle{cli: n.cli, rel: n.rel}, 0, 0
}

type fileHandle struct {
	cli *Client
	rel string
}

var _ fs.FileReader = (*fileHandle)(nil)

func (h *fileHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	data, err := h.cli.readRange(ctx, h.rel, off, int64(len(dest)))
	if err != nil {
		return nil, toErrno(err)
	}
	return fuse.ReadResultData(data), 0
}

func toErrno(err error) syscall.Errno {
	if err == nil {
		return 0
	}
	if err == syscall.ENOENT || strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "not found") {
		return syscall.ENOENT
	}
	return syscall.EIO
}
