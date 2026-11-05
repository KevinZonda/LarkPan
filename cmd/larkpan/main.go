// Command larkpan is a minimal driver for the drive package: list, mkdir,
// upload and download against a Feishu Drive folder with app identity.
//
// Credentials come from the environment:
//
//	LARK_APP_ID, LARK_APP_SECRET  (required)
//	LARK_DOMAIN                    "feishu" (default) or "lark"
package main

import (
	"context"
	"fmt"
	"os"

	"larkpan/internal/drive"
)

func usage() {
	fmt.Fprint(os.Stderr, `usage: larkpan <command> [args]

commands:
  list     [folder_token]            list folder children (empty = root)
  mkdir    <name> [parent_token]     create a folder
  upload   <file> [folder_token]     upload a file (auto-chunks >20MB)
  download <file_token> [output]     download a file
  rm       <token> [type]            delete a resource (type: file, folder, ...)
`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	c, err := drive.NewFromEnv()
	must(err)
	ctx := context.Background()

	switch os.Args[1] {
	case "list":
		var folder string
		if len(os.Args) > 2 {
			folder = os.Args[2]
		}
		files, err := c.ListFolder(ctx, folder)
		must(err)
		for _, f := range files {
			fmt.Printf("%-24s %-8s %s\n", val(f.Token), val(f.Type), val(f.Name))
		}
	case "mkdir":
		if len(os.Args) < 3 {
			usage()
		}
		parent := ""
		if len(os.Args) > 3 {
			parent = os.Args[3]
		}
		token, err := c.CreateFolder(ctx, os.Args[2], parent)
		must(err)
		fmt.Println(token)
	case "upload":
		if len(os.Args) < 3 {
			usage()
		}
		folder := ""
		if len(os.Args) > 3 {
			folder = os.Args[3]
		}
		res, err := c.Upload(ctx, os.Args[2], folder, "")
		must(err)
		fmt.Printf("%s (version=%s)\n", res.FileToken, res.Version)
	case "download":
		if len(os.Args) < 3 {
			usage()
		}
		out := "."
		if len(os.Args) > 3 {
			out = os.Args[3]
		}
		must(c.Download(ctx, os.Args[2], out))
	case "rm":
		if len(os.Args) < 3 {
			usage()
		}
		typ := "file"
		if len(os.Args) > 3 {
			typ = os.Args[3]
		}
		must(c.Delete(ctx, os.Args[2], typ))
	default:
		usage()
	}
}

func val(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
