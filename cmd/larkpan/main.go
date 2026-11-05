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
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"larkpan/internal/drive"
	"larkpan/internal/server"
	"larkpan/internal/sign"
)

func usage() {
	fmt.Fprint(os.Stderr, `usage: larkpan <command> [args]

commands:
  list     [folder_token]            list folder children (empty = root)
  mkdir    <name> [parent_token]     create a folder
  upload   <file> [folder_token]     upload a file (auto-chunks >20MB)
  download <file_token> [output]     download a file
  rm       <token> [type]            delete a resource (type: file, folder, ...)
  pull     <folder_token> <dir> [mode]
                                     mirror remote tree into local dir
                                     mode: smart (default) | skip | overwrite
  get      <folder_token> <rel_path>
                                     resolve + cache file, print local path
                                     (read-through cache; LARK_CACHE_DIR, LARK_META_TTL)
  serve    <folder_token> [addr]     serve the tree over HTTP
                                     /files/<path>  nginx-style files + JSON dir listing
                                     /s3/<bucket>/<key>  minimal S3 (bucket ignored, unauthenticated)
                                     default addr :8080
  token                            print a fresh tenant_access_token
  token-serve [addr]               vend tokens to sub-servers (GET /token)
                                     default addr :9090; auth: APP_ID/SECRET env
  sign     <path> [ttl_seconds]    emit a signed /files/ URL (default ttl 3600)
                                     requires LARKPAN_SIGN_SECRET; serve uses
                                     the same env to enforce signatures

credential modes (sub-servers):
  LARK_TENANT_TOKEN=t-...          use a delegated token directly
  LARK_TOKEN_URL=http://host:9090  fetch+refresh tokens from a vender
`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	ctx := context.Background()
	var c *drive.Client
	client := func() *drive.Client {
		if c == nil {
			var err error
			c, err = drive.NewFromEnv()
			must(err)
		}
		return c
	}

	switch os.Args[1] {
	case "list":
		var folder string
		if len(os.Args) > 2 {
			folder = os.Args[2]
		}
		files, err := client().ListFolder(ctx, folder)
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
		token, err := client().CreateFolder(ctx, os.Args[2], parent)
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
		res, err := client().Upload(ctx, os.Args[2], folder, "")
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
		must(client().Download(ctx, os.Args[2], out))
	case "rm":
		if len(os.Args) < 3 {
			usage()
		}
		typ := "file"
		if len(os.Args) > 3 {
			typ = os.Args[3]
		}
		must(client().Delete(ctx, os.Args[2], typ))
	case "pull":
		if len(os.Args) < 4 {
			usage()
		}
		mode := drive.PullSmart
		if len(os.Args) > 4 {
			mode = drive.PullMode(os.Args[4])
		}
		res, err := client().Pull(ctx, os.Args[2], os.Args[3], mode)
		must(err)
		for _, e := range res.Errors {
			fmt.Fprintf(os.Stderr, "failed: %s: %v\n", e.RelPath, e.Err)
		}
		fmt.Printf("downloaded=%d skipped=%d failed=%d conflicts=%d\n",
			res.Downloaded, res.Skipped, res.Failed, res.Conflicts)
		if res.Failed > 0 {
			os.Exit(1)
		}
	case "get":
		if len(os.Args) < 4 {
			usage()
		}
		cache, err := newCache(client())
		must(err)
		p, err := cache.Get(ctx, os.Args[2], os.Args[3])
		must(err)
		fmt.Println(p)
	case "serve":
		if len(os.Args) < 3 {
			usage()
		}
		addr := ":8080"
		if len(os.Args) > 3 {
			addr = os.Args[3]
		}
		cache, err := newCache(client())
		must(err)
		secret := os.Getenv("LARKPAN_SIGN_SECRET")
		srv := server.New(cache, os.Args[2], secret)
		fmt.Printf("serving %s on %s (auth=%v)\n", os.Args[2], addr, secret != "")
		must(http.ListenAndServe(addr, srv))
	case "sign":
		if len(os.Args) < 3 {
			usage()
		}
		secret := os.Getenv("LARKPAN_SIGN_SECRET")
		if secret == "" {
			fmt.Fprintln(os.Stderr, "sign requires LARKPAN_SIGN_SECRET")
			os.Exit(2)
		}
		ttl := int64(3600)
		if len(os.Args) > 3 {
			v, err := strconv.ParseInt(os.Args[3], 10, 64)
			must(err)
			ttl = v
		}
		exp := time.Now().Unix() + ttl
		p := "/files/" + strings.Trim(os.Args[2], "/")
		fmt.Printf("%s?expires=%d&sig=%s\n", p, exp, sign.Token(secret, p, exp))
		fmt.Fprintf(os.Stderr, "expires at %s\n", time.Unix(exp, 0))
	case "token":
		id, sec := os.Getenv("LARK_APP_ID"), os.Getenv("LARK_APP_SECRET")
		if id == "" || sec == "" {
			fmt.Fprintln(os.Stderr, "token requires LARK_APP_ID/LARK_APP_SECRET")
			os.Exit(2)
		}
		v := server.NewTokenVender(id, sec, apiBase())
		tok, _, err := v.Mint()
		must(err)
		fmt.Println(tok)
	case "token-serve":
		addr := ":9090"
		if len(os.Args) > 2 {
			addr = os.Args[2]
		}
		id, sec := os.Getenv("LARK_APP_ID"), os.Getenv("LARK_APP_SECRET")
		if id == "" || sec == "" {
			fmt.Fprintln(os.Stderr, "token-serve requires LARK_APP_ID/LARK_APP_SECRET")
			os.Exit(2)
		}
		v := server.NewTokenVender(id, sec, apiBase())
		fmt.Printf("vending tokens on %s\n", addr)
		must(http.ListenAndServe(addr, v))
	default:
		usage()
	}
}

// apiBase returns the OpenAPI base URL matching LARK_DOMAIN.
func apiBase() string {
	if os.Getenv("LARK_DOMAIN") == "lark" {
		return "https://open.larksuite.com"
	}
	return "https://open.feishu.cn"
}

// newCache builds the read-through cache from env:
// LARK_CACHE_DIR (default ./.larkpan-cache), LARK_META_TTL seconds (default 30).
func newCache(c *drive.Client) (*drive.Cache, error) {
	dir := os.Getenv("LARK_CACHE_DIR")
	if dir == "" {
		dir = ".larkpan-cache"
	}
	ttl := 30.0
	if s := os.Getenv("LARK_META_TTL"); s != "" {
		if v, err := strconv.ParseFloat(s, 64); err == nil {
			ttl = v
		}
	}
	return drive.NewCache(c, dir, time.Duration(ttl*float64(time.Second)))
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
