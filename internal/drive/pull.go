package drive

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// PullMode decides what to do when the local path already exists.
type PullMode string

const (
	// PullSmart skips files whose local mtime is at or past the remote
	// modified_time. This is the recommended repeated-sync mode.
	PullSmart PullMode = "smart"
	// PullSkip leaves every existing local file untouched.
	PullSkip PullMode = "skip"
	// PullOverwrite always downloads; remote is the authority.
	PullOverwrite PullMode = "overwrite"
)

// PullItemError records a per-file failure.
type PullItemError struct {
	RelPath string
	Err     error
}

// PullResult summarizes a pull run.
type PullResult struct {
	Downloaded int
	Skipped    int
	Failed     int
	Conflicts  int
	Errors     []PullItemError
}

type remoteFile struct {
	token    string
	relPath  string
	modified time.Time
}

// Pull mirrors the remote folder tree into localDir. Only Drive binary
// files (type=file) are fetched; online docs and shortcuts are skipped.
// Downloads run concurrently; each file lands via a temp rename so
// interrupted pulls never leave half-written files.
func (c *Client) Pull(ctx context.Context, folderToken, localDir string, mode PullMode) (*PullResult, error) {
	var remote []remoteFile
	if err := c.walk(ctx, folderToken, "", &remote); err != nil {
		return nil, err
	}

	// Deduplicate rel paths: remote siblings can collide after joining
	// (e.g. duplicate names); first seen wins, the rest count as conflicts.
	seen := make(map[string]bool, len(remote))
	res := &PullResult{}
	var jobs []remoteFile
	for _, rf := range remote {
		if seen[rf.relPath] {
			res.Conflicts++
			continue
		}
		seen[rf.relPath] = true

		dst := filepath.Join(localDir, filepath.FromSlash(rf.relPath))
		st, statErr := os.Stat(dst)
		exists := statErr == nil && !st.IsDir()
		switch {
		case exists && mode == PullSkip:
			res.Skipped++
		case exists && mode == PullSmart && !st.ModTime().Before(rf.modified):
			res.Skipped++
		default:
			jobs = append(jobs, rf)
		}
	}

	sem := make(chan struct{}, c.downloadConcurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, rf := range jobs {
		sem <- struct{}{}
		wg.Add(1)
		go func(rf remoteFile) {
			defer wg.Done()
			defer func() { <-sem }()
			dst := filepath.Join(localDir, filepath.FromSlash(rf.relPath))
			err := c.downloadOne(ctx, rf.token, dst)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				res.Failed++
				res.Errors = append(res.Errors, PullItemError{RelPath: rf.relPath, Err: err})
				return
			}
			// Stamp local mtime = remote modified_time so the next smart
			// pull can skip unchanged files.
			_ = os.Chtimes(dst, rf.modified, rf.modified)
			res.Downloaded++
		}(rf)
	}
	wg.Wait()
	return res, nil
}

// downloadOne writes a file to dst via a sibling temp file + rename.
func (c *Client) downloadOne(ctx context.Context, token, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".larkpan-part"
	if err := c.Download(ctx, token, tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// walk recursively lists all type=file entries under folderToken.
func (c *Client) walk(ctx context.Context, folderToken, prefix string, out *[]remoteFile) error {
	files, err := c.ListFolder(ctx, folderToken)
	if err != nil {
		return err
	}
	for _, f := range files {
		name := valStr(f.Name)
		typ := valStr(f.Type)
		switch typ {
		case "folder":
			if err := c.walk(ctx, valStr(f.Token), joinRel(prefix, name), out); err != nil {
				return err
			}
		case "file":
			*out = append(*out, remoteFile{
				token:    valStr(f.Token),
				relPath:  joinRel(prefix, name),
				modified: parseLarkTime(valStr(f.ModifiedTime)),
			})
		}
		// docx/sheet/bitable/slides/shortcut etc. have no binary body to mirror.
	}
	return nil
}

func joinRel(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "/" + name
}

// parseLarkTime accepts unix timestamps in seconds or milliseconds.
func parseLarkTime(s string) time.Time {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}
	}
	if n > 1e12 { // milliseconds
		return time.UnixMilli(n)
	}
	return time.Unix(n, 0)
}

func valStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
