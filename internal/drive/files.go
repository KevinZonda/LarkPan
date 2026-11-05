package drive

import (
	"context"
	"fmt"
	"io"
	"os"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkdrive "github.com/larksuite/oapi-sdk-go/v3/service/drive/v1"
)

// parentTypeExplorer targets a Drive folder as upload parent.
const parentTypeExplorer = "explorer"

// apiErr normalizes an SDK response into a single error value.
func apiErr(resp larkcore.CodeError) error {
	if resp.Code == 0 {
		return nil
	}
	return fmt.Errorf("lark api error: code=%d msg=%s", resp.Code, resp.Msg)
}

// ListFolder lists the direct children of a Drive folder, following pagination.
// An empty folderToken lists the app's own root directory listing.
func (c *Client) ListFolder(ctx context.Context, folderToken string) ([]*larkdrive.File, error) {
	var out []*larkdrive.File
	pageToken := ""
	for {
		resp, err := c.raw.Drive.File.List(ctx, larkdrive.NewListFileReqBuilder().
			FolderToken(folderToken).
			PageSize(200).
			PageToken(pageToken).
			Build())
		if err != nil {
			return nil, err
		}
		if err := apiErr(resp.CodeError); err != nil {
			return nil, err
		}
		out = append(out, resp.Data.Files...)
		if resp.Data.HasMore == nil || !*resp.Data.HasMore {
			return out, nil
		}
		pageToken = *resp.Data.NextPageToken
	}
}

// CreateFolder creates a folder named name under parentToken ("" = root).
func (c *Client) CreateFolder(ctx context.Context, name, parentToken string) (string, error) {
	resp, err := c.raw.Drive.File.CreateFolder(ctx, larkdrive.NewCreateFolderFileReqBuilder().
		Body(larkdrive.NewCreateFolderFileReqBodyBuilder().
			Name(name).
			FolderToken(parentToken).
			Build()).
		Build())
	if err != nil {
		return "", err
	}
	if err := apiErr(resp.CodeError); err != nil {
		return "", err
	}
	return *resp.Data.Token, nil
}

// Delete removes a Drive resource. typ is the API object type:
// "file" for binaries, "folder" for folders, plus doc/sheet/etc.
// Folder deletes run async server-side (Async=true).
func (c *Client) Delete(ctx context.Context, token, typ string) error {
	resp, err := c.raw.Drive.File.Delete(ctx, larkdrive.NewDeleteFileReqBuilder().
		FileToken(token).
		Type(typ).
		Async(typ == "folder").
		Build())
	if err != nil {
		return err
	}
	return apiErr(resp.CodeError)
}

// Download streams a Drive file to a local path.
func (c *Client) Download(ctx context.Context, fileToken, dstPath string) error {
	resp, err := c.raw.Drive.File.Download(ctx, larkdrive.NewDownloadFileReqBuilder().
		FileToken(fileToken).
		Build())
	if err != nil {
		return err
	}
	if err := apiErr(resp.CodeError); err != nil {
		return err
	}
	dst, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer dst.Close()
	_, err = io.Copy(dst, resp.File)
	return err
}
