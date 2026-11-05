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

	"larkpan/internal/client"
	"larkpan/internal/drive"
	"larkpan/internal/mount"
	"larkpan/internal/server"
	"larkpan/internal/sign"
	"larkpan/internal/smb"
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
  token-create <label> [ttl]       mint a long-lived client token (default 30d)
  client   login|mount|whoami      client-side auth + FUSE mount (linux)

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
		// Central+edge in one process: when app credentials are present,
		// the same listener also vends tokens on /token.
		var vend *server.TokenVender
		if id, sec := os.Getenv("LARK_APP_ID"), os.Getenv("LARK_APP_SECRET"); id != "" && sec != "" {
			vend = server.NewTokenVender(id, sec, apiBase(), os.Getenv("LARK_VEND_KEY"))
		}
		srv := server.New(cache, os.Args[2], secret, vend)
		fmt.Printf("serving %s on %s (auth=%v vend=%v)\n", os.Args[2], addr, secret != "", vend != nil)
		must(http.ListenAndServe(addr, srv))
	case "smb":
		// larkpan smb <folder_token> [addr] — share the tree over SMB2/3
		// (read-only). Credentials: LARK_SMB_USER / LARK_SMB_PASSWORD
		// (defaults larkpan/larkpan — set them!). Port 445 needs root.
		if len(os.Args) < 3 {
			usage()
		}
		addr := ":445"
		if len(os.Args) > 3 {
			addr = os.Args[3]
		}
		user := os.Getenv("LARK_SMB_USER")
		pw := os.Getenv("LARK_SMB_PASSWORD")
		if user == "" {
			user = "larkpan"
		}
		if pw == "" {
			pw = "larkpan"
		}
		cache, err := newCache(client())
		must(err)
		fmt.Printf("smb share \\\\host\\larkpan on %s (user %s)\n", addr, user)
		must(smb.Serve(addr, cache, os.Args[2], map[string]string{user: pw}))
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
		v := server.NewTokenVender(id, sec, apiBase(), os.Getenv("LARK_VEND_KEY"))
		tok, _, err := v.Mint()
		must(err)
		fmt.Println(tok)
	case "token-create":
		// larkpan token-create <label> [ttl_seconds] — mint a long-lived
		// client token signed by LARKPAN_SIGN_SECRET.
		if len(os.Args) < 3 {
			usage()
		}
		secret := os.Getenv("LARKPAN_SIGN_SECRET")
		if secret == "" {
			fmt.Fprintln(os.Stderr, "token-create requires LARKPAN_SIGN_SECRET")
			os.Exit(2)
		}
		ttl := int64(30 * 24 * 3600) // 30 days
		if len(os.Args) > 3 {
			v, err := strconv.ParseInt(os.Args[3], 10, 64)
			must(err)
			ttl = v
		}
		exp := time.Now().Unix() + ttl
		fmt.Println(sign.ClientToken(secret, os.Args[2], exp))
		fmt.Fprintf(os.Stderr, "expires at %s\n", time.Unix(exp, 0))
	case "client":
		clientCmd(ctx)
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
		v := server.NewTokenVender(id, sec, apiBase(), os.Getenv("LARK_VEND_KEY"))
		fmt.Printf("vending tokens on %s\n", addr)
		must(http.ListenAndServe(addr, v))
	default:
		usage()
	}
}

// clientCmd implements `larkpan client <sub>`:
//
//	client login --server <url> --token <lp_...>   save credentials to
//	                                               ~/.config/kevinzonda.larkpan/token
//	client mount <dir>                             FUSE-mount the remote tree
//	                                               (linux only)
//	client whoami                                  show saved config
func clientCmd(ctx context.Context) {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: larkpan client login|mount|whoami ...")
		os.Exit(2)
	}
	switch os.Args[2] {
	case "login":
		var srvURL, tok string
		for i := 3; i+1 < len(os.Args); i += 2 {
			switch os.Args[i] {
			case "--server":
				srvURL = os.Args[i+1]
			case "--token":
				tok = os.Args[i+1]
			}
		}
		if srvURL == "" || tok == "" {
			fmt.Fprintln(os.Stderr, "login requires --server and --token")
			os.Exit(2)
		}
		cfg := &client.Config{Server: strings.TrimRight(srvURL, "/"), Token: tok}
		must(cfg.Save())
		p, _ := client.Path()
		fmt.Println("saved to", p)
	case "mount":
		if len(os.Args) < 4 {
			fmt.Fprintln(os.Stderr, "usage: larkpan client mount <dir>")
			os.Exit(2)
		}
		cfg, err := client.Load()
		must(err)
		fmt.Printf("mounting %s at %s (ctrl-c to unmount)\n", cfg.Server, os.Args[3])
		must(mount.Mount(&mount.Client{Base: cfg.Server, Token: cfg.Token}, os.Args[3]))
	case "whoami":
		cfg, err := client.Load()
		must(err)
		fmt.Printf("server: %s\ntoken:  %s...%s\n", cfg.Server, cfg.Token[:min(10, len(cfg.Token))], cfg.Token[max(0, len(cfg.Token)-8):])
	default:
		fmt.Fprintln(os.Stderr, "usage: larkpan client login|mount|whoami ...")
		os.Exit(2)
	}
	_ = ctx
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
