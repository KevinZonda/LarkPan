package drive

import (
	"bytes"
	"context"
	"fmt"
	"hash/adler32"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	larkdrive "github.com/larksuite/oapi-sdk-go/v3/service/drive/v1"
)

// simpleUploadLimit is the upload_all size ceiling; larger files go chunked.
const simpleUploadLimit = 20 << 20 // 20 MiB

// UploadResult reports a completed upload.
type UploadResult struct {
	FileToken string
	Version   string
}

// Upload writes localPath into folderToken ("" = root) under name
// ("" = local basename). It picks upload_all for small files and the
// prepare/part/finish chunked flow for anything larger.
func (c *Client) Upload(ctx context.Context, localPath, folderToken, name string) (*UploadResult, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if name == "" {
		name = filepath.Base(localPath)
	}
	if st.Size() <= simpleUploadLimit {
		return c.uploadAll(ctx, f, st.Size(), name, folderToken)
	}
	return c.uploadChunked(ctx, f, st.Size(), name, folderToken)
}

func (c *Client) uploadAll(ctx context.Context, r io.Reader, size int64, name, folderToken string) (*UploadResult, error) {
	buf, err := io.ReadAll(io.LimitReader(r, simpleUploadLimit))
	if err != nil {
		return nil, err
	}
	resp, err := c.raw.Drive.File.UploadAll(ctx, larkdrive.NewUploadAllFileReqBuilder().
		Body(larkdrive.NewUploadAllFileReqBodyBuilder().
			FileName(name).
			ParentType(parentTypeExplorer).
			ParentNode(folderToken).
			Size(int(size)).
			Checksum(adlerSum(buf)).
			File(bytes.NewReader(buf)).
			Build()).
		Build())
	if err != nil {
		return nil, err
	}
	if err := apiErr(resp.CodeError); err != nil {
		return nil, err
	}
	return &UploadResult{FileToken: *resp.Data.FileToken}, nil
}

func (c *Client) uploadChunked(ctx context.Context, f *os.File, size int64, name, folderToken string) (*UploadResult, error) {
	prep, err := c.raw.Drive.File.UploadPrepare(ctx, larkdrive.NewUploadPrepareFileReqBuilder().
		FileUploadInfo(larkdrive.NewFileUploadInfoBuilder().
			FileName(name).
			ParentType(parentTypeExplorer).
			ParentNode(folderToken).
			Size(int(size)).
			Build()).
		Build())
	if err != nil {
		return nil, err
	}
	if err := apiErr(prep.CodeError); err != nil {
		return nil, err
	}
	uploadID := *prep.Data.UploadId
	blockSize := int64(*prep.Data.BlockSize)
	blockNum := *prep.Data.BlockNum

	// Upload parts concurrently. The first failure cancels the rest and is
	// returned after all in-flight parts settle.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, min(c.uploadConcurrency, blockNum))
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
		cancel()
	}
	failed := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return firstErr != nil
	}

	for seq := 0; seq < blockNum; seq++ {
		if failed() {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(seq int) {
			defer wg.Done()
			defer func() { <-sem }()
			want := min(blockSize, size-int64(seq)*blockSize)
			buf := make([]byte, want)
			if _, err := f.ReadAt(buf, int64(seq)*blockSize); err != nil {
				fail(fmt.Errorf("read block %d: %w", seq, err))
				return
			}
			for attempt := 0; ; attempt++ {
				resp, err := c.raw.Drive.File.UploadPart(ctx, larkdrive.NewUploadPartFileReqBuilder().
					Body(larkdrive.NewUploadPartFileReqBodyBuilder().
						UploadId(uploadID).
						Seq(seq).
						Size(int(want)).
						Checksum(adlerSum(buf)).
						File(bytes.NewReader(buf)).
						Build()).
					Build())
				if err == nil && resp.Code == 0 {
					break
				}
				if !isRateLimited(resp, err) || attempt >= 3 {
					if err == nil {
						err = apiErr(resp.CodeError)
					}
					fail(fmt.Errorf("upload part %d/%d: %w", seq+1, blockNum, err))
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Duration(1<<attempt) * time.Second):
				}
			}
		}(seq)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}

	fin, err := c.raw.Drive.File.UploadFinish(ctx, larkdrive.NewUploadFinishFileReqBuilder().
		Body(larkdrive.NewUploadFinishFileReqBodyBuilder().
			UploadId(uploadID).
			BlockNum(blockNum).
			Build()).
		Build())
	if err != nil {
		return nil, err
	}
	if err := apiErr(fin.CodeError); err != nil {
		return nil, err
	}
	res := &UploadResult{FileToken: *fin.Data.FileToken}
	if fin.Data.Version != nil {
		res.Version = *fin.Data.Version
	}
	return res, nil
}

// isRateLimited reports whether a failed part upload was throttled.
// Feishu signals per-API frequency caps with code 99991400.
func isRateLimited(resp *larkdrive.UploadPartFileResp, err error) bool {
	if resp != nil && resp.Code == 99991400 {
		return true
	}
	return err != nil && strings.Contains(err.Error(), "frequency limit")
}

// adlerSum returns the Adler-32 checksum the drive API expects, decimal.
func adlerSum(b []byte) string {
	return fmt.Sprintf("%d", adler32.Checksum(b))
}
