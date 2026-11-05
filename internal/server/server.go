// Package server exposes the drive read-through cache over HTTP:
// an nginx-style file endpoint (/files/) and a minimal S3-compatible
// endpoint (/s3/).
package server

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/webdav"

	"larkpan/internal/drive"
	"larkpan/internal/sign"
)

// Server serves a single remote Drive folder tree.
type Server struct {
	cache  *drive.Cache
	root   string // root folder token
	secret string // edge signing secret; "" = open access
	mux    *http.ServeMux
}

// New builds a Server exposing the tree rooted at rootFolderToken.
// secret enables signed-URL auth on /files/ ("" = open access).
// /s3/ stays unauthenticated — protect it with a network boundary.
func New(cache *drive.Cache, rootFolderToken, secret string) *Server {
	s := &Server{cache: cache, root: rootFolderToken, secret: secret, mux: http.NewServeMux()}
	s.mux.HandleFunc("/healthz", s.health)
	s.mux.HandleFunc("/files/", s.guard(s.handleFiles))
	s.mux.HandleFunc("/json/", s.guard(s.handleJSON))
	s.mux.HandleFunc("/refresh", s.guard(s.handleRefresh))
	s.mux.HandleFunc("/s3/", s.handleS3)
	s.mux.HandleFunc("/", s.index)
	// WebDAV mount point (read-only): net use Z: http://host/dav/
	s.mux.Handle("/dav/", s.guard((&webdav.Handler{
		Prefix:     "/dav",
		FileSystem: newDAVFS(cache, rootFolderToken),
		LockSystem: webdav.NewMemLS(),
	}).ServeHTTP))
	return s
}

// guard enforces auth when a secret is configured: either a signed URL
// (?expires&sig) or a bearer client token (Authorization: Bearer lp_...).
func (s *Server) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.secret == "" {
			h(w, r)
			return
		}
		now := time.Now()
		if b := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); b != "" && sign.ValidBearer(s.secret, b, now) {
			h(w, r)
			return
		}
		// net use / WebDAV clients send Basic credentials after a 401
		// challenge; accept an lp_ token in the password field.
		if _, pw, ok := r.BasicAuth(); ok && sign.ValidBearer(s.secret, pw, now) {
			h(w, r)
			return
		}
		if sign.Valid(s.secret, r, now) {
			h(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="larkpan"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.Printf("%s %s", r.Method, r.URL)
	// Windows WebClient (net use) probes OPTIONS on the server root to
	// detect DAV support; answer globally so mounts resolve.
	if r.Method == http.MethodOptions {
		w.Header().Set("DAV", "1,2")
		w.Header().Set("MS-Author-Via", "DAV")
		w.Header().Set("Allow", "OPTIONS, PROPFIND, GET, HEAD")
		w.WriteHeader(http.StatusOK)
		return
	}
	s.mux.ServeHTTP(w, r)
}

// handleRefresh re-signs a live bearer token with a fresh 30d expiry.
// Only reachable past guard() — an expired token cannot refresh.
func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	b := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	label := sign.BearerLabel(b)
	if label == "" {
		http.Error(w, "malformed token", http.StatusBadRequest)
		return
	}
	exp := time.Now().Add(30 * 24 * time.Hour).Unix()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"token":   sign.ClientToken(s.secret, label, exp),
		"expires": exp,
	})
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ok")
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>larkpan</title>
<h1>larkpan</h1>
<p>Serving Feishu Drive folder <code>%s</code> through a read-through local cache.</p>
<ul>
<li><a href="/files/">/files/</a> — nginx-style file browsing (autoindex + Range)</li>
<li><a href="/json/">/json/</a> — JSON directory listings / object metadata</li>
<li>/s3/&lt;bucket&gt;/&lt;key&gt; — S3-compatible: <a href="/s3/weights?list-type=2">ListObjectsV2</a>, GetObject, HeadObject</li>
<li><a href="/healthz">/healthz</a></li>
</ul>`, s.root)
}

// --- nginx-style file endpoint -------------------------------------------

// handleFiles serves GET/HEAD /files/<path> like an nginx file root:
// directories render an autoindex HTML page, files are served from cache
// with Range support.
func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rel := strings.TrimPrefix(r.URL.Path, "/files/")
	rel = strings.Trim(rel, "/")

	if rel == "" || strings.HasSuffix(r.URL.Path, "/") {
		s.autoIndex(w, r, rel)
		return
	}

	local, err := s.cache.Get(r.Context(), s.root, rel)
	if errors.Is(err, drive.ErrIsDir) {
		http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
		return
	}
	if !serveErr(w, err, rel) {
		return
	}
	s.serveFile(w, r, rel, local)
}

// autoIndex renders an nginx-style directory index page.
func (s *Server) autoIndex(w http.ResponseWriter, r *http.Request, rel string) {
	ents, err := s.cache.ReadDir(r.Context(), s.root, rel)
	if !serveErr(w, err, rel) {
		return
	}
	sort.Slice(ents, func(i, j int) bool {
		if ents[i].IsDir != ents[j].IsDir {
			return ents[i].IsDir // dirs first
		}
		return ents[i].Name < ents[j].Name
	})

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	base := "/files/" + rel
	if rel != "" {
		base += "/"
	}
	fmt.Fprintf(w, "<html><head><title>Index of %s</title></head><body>\n<h1>Index of %s</h1><hr><pre>\n", base, base)
	if rel != "" {
		fmt.Fprintln(w, `<a href="../">../</a>`)
	}
	for _, e := range ents {
		name, href, size := e.Name, urlPathEscape(e.Name), fmt.Sprint(e.Size)
		if e.IsDir {
			name += "/"
			href += "/"
			size = "-"
		}
		mod := "-"
		if !e.Modified.IsZero() {
			mod = e.Modified.Format("02-Jan-2006 15:04")
		}
		fmt.Fprintf(w, `<a href="%s%s">%s</a>%s%s  %s`+"\n",
			base, href, htmlEscape(name),
			strings.Repeat(" ", max(2, 50-len(name))), mod, size)
	}
	fmt.Fprintln(w, "</pre><hr></body></html>")
}

// handleJSON serves machine-readable listings under /json/<path>:
// directories return an entry array, files return a metadata object.
func (s *Server) handleJSON(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rel := strings.TrimPrefix(r.URL.Path, "/json/")
	rel = strings.Trim(rel, "/")

	ents, err := s.cache.ReadDir(r.Context(), s.root, rel)
	if err == nil {
		s.writeJSONList(w, ents)
		return
	}
	if errors.Is(err, drive.ErrNotFound) {
		// Maybe a file: fall back to metadata.
		st, serr := s.cache.Stat(r.Context(), s.root, rel)
		if serr != nil {
			serveErr(w, err, rel)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"name": st.Name, "type": "file", "token": st.Token, "modified": st.Modified,
		})
		return
	}
	serveErr(w, err, rel)
}

func (s *Server) writeJSONList(w http.ResponseWriter, ents []drive.DirEntry) {
	type item struct {
		Name     string    `json:"name"`
		Type     string    `json:"type"`
		Size     int64     `json:"size,omitempty"`
		Modified time.Time `json:"modified"`
	}
	items := make([]item, 0, len(ents))
	for _, e := range ents {
		t := "file"
		if e.IsDir {
			t = "dir"
		}
		items = append(items, item{Name: e.Name, Type: t, Size: e.Size, Modified: e.Modified})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(items)
}

// serveFile streams a cached local file; http.ServeContent provides
// Range, If-Modified-Since and HEAD handling for free.
func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, name, local string) {
	f, err := os.Open(local)
	if err != nil {
		serveErr(w, err, name)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		serveErr(w, err, name)
		return
	}
	http.ServeContent(w, r, st.Name(), st.ModTime(), f)
}

// --- S3-compatible endpoint ------------------------------------------------
//
// Minimal read-only S3 over /s3/<bucket>/<key>. Any bucket name maps to the
// configured root folder. Supported: GetObject (with Range via the local
// cache file), HeadObject, ListObjectsV2. SigV4 auth is not verified (TODO).

const s3Prefix = "/s3/"

func (s *Server) handleS3(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, s3Prefix)
	bucket, key, _ := strings.Cut(rest, "/")
	if bucket == "" {
		s.s3Error(w, http.StatusBadRequest, "InvalidRequest", "missing bucket", r.URL.Path)
		return
	}
	if key == "" || r.URL.Query().Has("list-type") {
		s.listObjects(w, r, bucket)
		return
	}
	s.getObject(w, r, bucket, key)
}

func (s *Server) getObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		s.s3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "only GET/HEAD supported", key)
		return
	}
	key = path.Clean("/" + key)[1:] // normalize, prevent escaping the root
	st, err := s.cache.Stat(r.Context(), s.root, key)
	if errors.Is(err, drive.ErrNotFound) {
		s.s3Error(w, http.StatusNotFound, "NoSuchKey", "key not found", key)
		return
	}
	if err != nil {
		s.s3Error(w, http.StatusInternalServerError, "InternalError", err.Error(), key)
		return
	}
	local, err := s.cache.Get(r.Context(), s.root, key)
	if err != nil {
		s.s3Error(w, http.StatusInternalServerError, "InternalError", err.Error(), key)
		return
	}
	f, err := os.Open(local)
	if err != nil {
		s.s3Error(w, http.StatusInternalServerError, "InternalError", err.Error(), key)
		return
	}
	defer f.Close()
	fst, err := f.Stat()
	if err != nil {
		s.s3Error(w, http.StatusInternalServerError, "InternalError", err.Error(), key)
		return
	}
	w.Header().Set("ETag", `"`+st.Token+`"`)
	http.ServeContent(w, r, st.Name, fst.ModTime(), f)
}

type listItem struct {
	key      string
	modified time.Time
	etag     string
	size     int64
	isPrefix bool
}

func (s *Server) listObjects(w http.ResponseWriter, r *http.Request, bucket string) {
	q := r.URL.Query()
	prefix := q.Get("prefix")
	delimiter := q.Get("delimiter")
	maxKeys, _ := strconv.Atoi(q.Get("max-keys"))
	if maxKeys <= 0 || maxKeys > 1000 {
		maxKeys = 1000
	}
	var offset int
	if t := q.Get("continuation-token"); t != "" {
		offset, _ = strconv.Atoi(t)
	}
	startAfter := q.Get("start-after")

	var items []listItem
	var err error
	if delimiter == "/" {
		items, err = s.listLevel(r.Context(), prefix)
	} else {
		items, err = s.listRecursive(r.Context(), prefix)
	}
	if err != nil {
		s.s3Error(w, http.StatusInternalServerError, "InternalError", err.Error(), r.URL.Path)
		return
	}
	sort.Slice(items, func(i, j int) bool {
		ai, aj := items[i].key, items[j].key
		if items[i].isPrefix {
			ai += "/"
		}
		if items[j].isPrefix {
			aj += "/"
		}
		return ai < aj
	})
	if startAfter != "" {
		items = filterAfter(items, startAfter)
	}
	if offset > 0 {
		items = items[min(offset, len(items)):]
	}
	truncated := false
	nextToken := ""
	if len(items) > maxKeys {
		truncated = true
		items = items[:maxKeys]
		nextToken = strconv.Itoa(offset + maxKeys)
	}

	type s3Content struct {
		Key          string `xml:"Key"`
		LastModified string `xml:"LastModified"`
		ETag         string `xml:"ETag"`
		Size         int64  `xml:"Size"`
		StorageClass string `xml:"StorageClass"`
	}
	type s3CommonPrefix struct {
		Prefix string `xml:"Prefix"`
	}
	out := struct {
		XMLName               xml.Name         `xml:"ListBucketResult"`
		Xmlns                 string           `xml:"xmlns,attr"`
		Name                  string           `xml:"Name"`
		Prefix                string           `xml:"Prefix"`
		KeyCount              int              `xml:"KeyCount"`
		MaxKeys               int              `xml:"MaxKeys"`
		Delimiter             string           `xml:"Delimiter,omitempty"`
		IsTruncated           bool             `xml:"IsTruncated"`
		Contents              []s3Content      `xml:"Contents"`
		CommonPrefixes        []s3CommonPrefix `xml:"CommonPrefixes"`
		NextContinuationToken string           `xml:"NextContinuationToken,omitempty"`
	}{
		Xmlns:                 "http://s3.amazonaws.com/doc/2006-03-01/",
		Name:                  bucket,
		Prefix:                prefix,
		KeyCount:              len(items),
		MaxKeys:               maxKeys,
		Delimiter:             delimiter,
		IsTruncated:           truncated,
		NextContinuationToken: nextToken,
	}
	for _, it := range items {
		if it.isPrefix {
			out.CommonPrefixes = append(out.CommonPrefixes, s3CommonPrefix{Prefix: it.key + "/"})
			continue
		}
		out.Contents = append(out.Contents, s3Content{
			Key:          it.key,
			LastModified: it.modified.UTC().Format(time.RFC3339),
			ETag:         `"` + it.etag + `"`,
			Size:         it.size,
			StorageClass: "STANDARD",
		})
	}
	w.Header().Set("Content-Type", "application/xml")
	xml.NewEncoder(w).Encode(out)
}

// listLevel lists one directory level under prefix ("<a>/<b>/").
func (s *Server) listLevel(ctx context.Context, prefix string) ([]listItem, error) {
	dir := strings.TrimSuffix(prefix, "/")
	ents, err := s.cache.ReadDir(ctx, s.root, dir)
	if err != nil {
		return nil, err
	}
	base := ""
	if dir != "" {
		base = dir + "/"
	}
	var out []listItem
	for _, e := range ents {
		out = append(out, listItem{
			key:      base + e.Name,
			modified: e.Modified,
			etag:     e.Token,
			size:     e.Size,
			isPrefix: e.IsDir,
		})
	}
	return out, nil
}

// listRecursive flattens all files under prefix.
func (s *Server) listRecursive(ctx context.Context, prefix string) ([]listItem, error) {
	dir := strings.TrimSuffix(prefix, "/")
	var out []listItem
	var walk func(rel string) error
	walk = func(rel string) error {
		ents, err := s.cache.ReadDir(ctx, s.root, rel)
		if err != nil {
			return err
		}
		for _, e := range ents {
			p := joinSlash(rel, e.Name)
			if e.IsDir {
				if err := walk(p); err != nil {
					return err
				}
				continue
			}
			out = append(out, listItem{key: p, modified: e.Modified, etag: e.Token, size: e.Size})
		}
		return nil
	}
	if err := walk(dir); err != nil {
		return nil, err
	}
	return out, nil
}

func filterAfter(items []listItem, startAfter string) []listItem {
	out := items[:0]
	for _, it := range items {
		if it.key > startAfter {
			out = append(out, it)
		}
	}
	return out
}

func joinSlash(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

func (s *Server) s3Error(w http.ResponseWriter, status int, code, msg, resource string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message><Resource>%s</Resource></Error>`,
		xmlEscape(code), xmlEscape(msg), xmlEscape(resource))
}

// serveErr writes an HTTP error for err and reports whether handling should
// stop (true = response already written).
func serveErr(w http.ResponseWriter, err error, resource string) bool {
	if err == nil {
		return true
	}
	switch {
	case errors.Is(err, drive.ErrNotFound):
		http.Error(w, "not found: "+resource, http.StatusNotFound)
	case errors.Is(err, drive.ErrIsDir):
		http.Error(w, "is a directory: "+resource, http.StatusBadRequest)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
	return false
}

func xmlEscape(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

func htmlEscape(s string) string    { return html.EscapeString(s) }
func urlPathEscape(s string) string { return url.PathEscape(s) }
