// Package smb adapts the read-through drive cache to a SMB2/3 share via
// github.com/macos-fuse-t/go-smb2 (AGPLv3). Read-only: all mutating
// calls return an error.
package smb

import (
	"context"
	"errors"
	"hash/fnv"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	smbsrv "github.com/macos-fuse-t/go-smb2/server"
	"github.com/macos-fuse-t/go-smb2/vfs"

	"larkpan/internal/drive"
)

var (
	errRO       = errors.New("read-only filesystem")
	errNotFound = fs.ErrNotExist
)

// FS implements vfs.VFSFileSystem over a drive.Cache.
type FS struct {
	cache *drive.Cache
	root  string

	next atomic.Uint64
	mu   sync.Mutex
	hand map[vfs.VfsHandle]*handle
}

type handle struct {
	rel  string
	file *os.File // non-nil for open files (local cache path)
	dir  bool
	ents []vfs.DirInfo
	idx  int
}

// NewFS builds a read-only share rooted at the remote folder token.
func NewFS(cache *drive.Cache, rootFolderToken string) *FS {
	return &FS{cache: cache, root: rootFolderToken, hand: map[vfs.VfsHandle]*handle{}}
}

// Serve runs an SMB server on addr sharing the tree as share "larkpan".
// auth is a username->password map for NTLMv2 (use lp_ tokens or any creds).
func Serve(addr string, cache *drive.Cache, rootFolderToken string, auth map[string]string) error {
	a := &smbsrv.NTLMAuthenticator{UserPassword: auth}
	srv := smbsrv.NewServer(&smbsrv.ServerConfig{IgnoreSetAttrErr: true}, a,
		map[string]vfs.VFSFileSystem{"larkpan": NewFS(cache, rootFolderToken)})
	return srv.Serve(addr)
}

// --- helpers ----------------------------------------------------------------

func (s *FS) put(h *handle) vfs.VfsHandle {
	id := vfs.VfsHandle(s.next.Add(1))
	s.mu.Lock()
	s.hand[id] = h
	s.mu.Unlock()
	return id
}

func (s *FS) get(h vfs.VfsHandle) (*handle, error) {
	if h == 0 {
		return &handle{rel: "", dir: true}, nil // handle 0 = share root
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if x, ok := s.hand[h]; ok {
		return x, nil
	}
	return nil, os.ErrInvalid
}

func (s *FS) del(h vfs.VfsHandle) {
	s.mu.Lock()
	delete(s.hand, h)
	s.mu.Unlock()
}

func attrs(rel string, isDir bool, size int64, mtime time.Time) *vfs.Attributes {
	h := fnv.New64a()
	h.Write([]byte(rel))
	id := h.Sum64()
	a := &vfs.Attributes{}
	if isDir {
		a.SetFileType(vfs.FileTypeDirectory)
		a.SetUnixMode(0o555 | uint32(fs.ModeDir))
	} else {
		a.SetFileType(vfs.FileTypeRegularFile)
		a.SetUnixMode(0o444)
	}
	a.SetInodeNumber(id)
	a.SetFileHandle(vfs.VfsNode(id))
	a.SetLinkCount(1)
	a.SetSizeBytes(uint64(size))
	a.SetDiskSizeBytes(uint64(size))
	a.SetLastDataModificationTime(mtime)
	a.SetLastStatusChangeTime(mtime)
	a.SetBirthTime(mtime)
	return a
}

func (s *FS) statRel(rel string) (*vfs.Attributes, bool, error) {
	// File?
	e, err := s.cache.Stat(context.Background(), s.root, rel)
	if err == nil {
		return attrs(rel, false, e.Size, e.Modified), false, nil
	}
	if errors.Is(err, drive.ErrIsDir) {
		return attrs(rel, true, 0, time.Now()), true, nil
	}
	// Might still be a dir (Stat errors on dirs in some paths).
	if rel == "" {
		return attrs(rel, true, 0, time.Now()), true, nil
	}
	if _, lerr := s.cache.ReadDir(context.Background(), s.root, rel); lerr == nil {
		return attrs(rel, true, 0, time.Now()), true, nil
	}
	return nil, false, errNotFound
}

// --- VFSFileSystem -----------------------------------------------------------

// norm converts an SMB path ("\\demo\\x", "demo/x", ".") to our rel path.
func norm(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	p = strings.Trim(p, "/")
	if p == "." {
		return ""
	}
	return p
}

func (s *FS) Open(p string, flags int, mode int) (vfs.VfsHandle, error) {
	if flags&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0 {
		return 0, errRO
	}
	rel := norm(p)
	local, err := s.cache.Get(context.Background(), s.root, rel)
	if err != nil {
		return 0, errNotFound
	}
	f, err := os.Open(local)
	if err != nil {
		return 0, errNotFound
	}
	return s.put(&handle{rel: rel, file: f}), nil
}

func (s *FS) OpenDir(p string) (vfs.VfsHandle, error) {
	rel := norm(p)
	ents, err := s.cache.ReadDir(context.Background(), s.root, rel)
	if err != nil {
		return 0, errNotFound
	}
	h := &handle{rel: rel, dir: true}
	for _, e := range ents {
		h.ents = append(h.ents, vfs.DirInfo{
			Name:       e.Name,
			Attributes: *attrs(path.Join(rel, e.Name), e.IsDir, e.Size, e.Modified),
		})
	}
	return s.put(h), nil
}

func (s *FS) Read(h vfs.VfsHandle, buf []byte, off uint64, flags int) (int, error) {
	x, err := s.get(h)
	if err != nil || x.file == nil {
		return 0, os.ErrInvalid
	}
	n, err := x.file.ReadAt(buf, int64(off))
	if err == io.EOF {
		err = nil
	}
	return n, err
}

func (s *FS) ReadDir(h vfs.VfsHandle, flags int, maxEntries int) ([]vfs.DirInfo, error) {
	x, err := s.get(h)
	if err != nil || !x.dir {
		return nil, os.ErrInvalid
	}
	if x.ents == nil {
		ents, err := s.cache.ReadDir(context.Background(), s.root, x.rel)
		if err != nil {
			return nil, errNotFound
		}
		for _, e := range ents {
			x.ents = append(x.ents, vfs.DirInfo{
				Name:       e.Name,
				Attributes: *attrs(path.Join(x.rel, e.Name), e.IsDir, e.Size, e.Modified),
			})
		}
	}
	if flags == vfs.ReadDirRestart {
		x.idx = 0
	}
	if x.idx >= len(x.ents) {
		return nil, nil
	}
	end := x.idx + maxEntries
	if maxEntries <= 0 || end > len(x.ents) {
		end = len(x.ents)
	}
	out := x.ents[x.idx:end]
	x.idx = end
	return out, nil
}

func (s *FS) Lookup(h vfs.VfsHandle, name string) (*vfs.Attributes, error) {
	x, err := s.get(h)
	if err != nil || !x.dir {
		return nil, os.ErrInvalid
	}
	a, _, err := s.statRel(path.Join(x.rel, name))
	return a, err
}

func (s *FS) GetAttr(h vfs.VfsHandle) (*vfs.Attributes, error) {
	x, err := s.get(h)
	if err != nil {
		return nil, err
	}
	a, _, err := s.statRel(x.rel)
	return a, err
}

func (s *FS) Close(h vfs.VfsHandle) error {
	x, err := s.get(h)
	if err != nil {
		return err
	}
	s.del(h)
	if x.file != nil {
		x.file.Close()
	}
	return nil
}

func (s *FS) StatFS(vfs.VfsHandle) (*vfs.FSAttributes, error) {
	return &vfs.FSAttributes{}, nil
}

func (s *FS) FSync(vfs.VfsHandle) error                 { return nil }
func (s *FS) Flush(vfs.VfsHandle) error                 { return nil }
func (s *FS) Readlink(vfs.VfsHandle) (string, error)    { return "", errNotFound }
func (s *FS) Listxattr(vfs.VfsHandle) ([]string, error) { return nil, nil }
func (s *FS) Getxattr(vfs.VfsHandle, string, []byte) (int, error) {
	return 0, errNotFound
}
func (s *FS) Setxattr(vfs.VfsHandle, string, []byte) error { return errRO }
func (s *FS) Removexattr(vfs.VfsHandle, string) error      { return errRO }
func (s *FS) SetAttr(_ vfs.VfsHandle, a *vfs.Attributes) (*vfs.Attributes, error) {
	return a, errRO
}
func (s *FS) Mkdir(string, int) (*vfs.Attributes, error) { return nil, errRO }
func (s *FS) Write(vfs.VfsHandle, []byte, uint64, int) (int, error) {
	return 0, errRO
}
func (s *FS) Unlink(vfs.VfsHandle) error              { return errRO }
func (s *FS) Truncate(vfs.VfsHandle, uint64) error    { return errRO }
func (s *FS) Rename(vfs.VfsHandle, string, int) error { return errRO }
func (s *FS) Symlink(vfs.VfsHandle, string, int) (*vfs.Attributes, error) {
	return nil, errRO
}
func (s *FS) Link(vfs.VfsNode, vfs.VfsNode, string) (*vfs.Attributes, error) {
	return nil, errRO
}
