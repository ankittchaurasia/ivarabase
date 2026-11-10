package core

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"
)

// Resumable uploads: TUS 1.0.0 at /storage/v1/upload/resumable, the endpoint
// Supabase documents for tus-js-client and Uppy. An upload is a state file plus
// a partial data file under <data>/tmp/tus/<project>/, so it survives a server
// restart. The object row is written (as the caller's role, through the same
// storeObject as a plain upload) only when the last byte arrives.

const (
	tusVersion   = "1.0.0"
	tusExtension = "creation,creation-with-upload,termination,expiration"
	tusExpiry    = 24 * time.Hour
	tusLockWait  = 10 * time.Second
)

type tusUpload struct {
	ID           string         `json:"id"`      // base64url(bucket/name/version): the Location id, like Supabase's
	Version      string         `json:"version"` // uuid, names the files on disk
	Project      string         `json:"project"`
	Bucket       string         `json:"bucket"`
	Name         string         `json:"name"`
	Length       int64          `json:"length"`
	ContentType  string         `json:"content_type"`
	CacheControl string         `json:"cache_control"`
	UserMeta     map[string]any `json:"user_metadata,omitempty"`
	RawMeta      string         `json:"raw_metadata"`
	Upsert       bool           `json:"upsert"`
	Role         string         `json:"role"` // the creator: only the same role and sub may continue it
	Sub          string         `json:"sub"`
	Created      time.Time      `json:"created"`
	Expires      time.Time      `json:"expires"`
}

func tusPath(sub string) bool {
	return sub == "/upload/resumable" || strings.HasPrefix(sub, "/upload/resumable/")
}

var tusVersionRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func (a *App) tusDir(slug string) string { return a.Cfg.Path("tmp", "tus", slug) }

func (a *App) tusFile(slug, version, ext string) string {
	return filepath.Join(a.tusDir(slug), version+ext)
}

var errUploadNotFound = storageErr(404, "not_found", "Upload not found")

// tusOptions answers OPTIONS (also a CORS preflight) with the server's limits.
func (a *App) tusOptions(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Tus-Resumable", tusVersion)
	h.Set("Tus-Version", tusVersion)
	h.Set("Tus-Extension", tusExtension)
	h.Set("Tus-Max-Size", strconv.FormatInt(int64(a.storageConfig().MaxFileSizeMB)<<20, 10))
}

// serveTUS handles /storage/v1/upload/resumable[/<id>]. The gateway lets these
// through without an apikey (Supabase's examples send only authorization), so
// the bearer token - or the apikey when there is no bearer - is checked here.
func (x *storeReq) serveTUS(ctx context.Context, sub string) {
	h := x.w.Header()
	h.Set("Tus-Resumable", tusVersion)
	h.Set("Cache-Control", "no-store")
	m := x.r.Method
	if o := x.r.Header.Get("X-HTTP-Method-Override"); m == "POST" && o != "" {
		m = strings.ToUpper(o) // tus-js-client's overridePatchMethod
	}
	fail := func(err error) {
		if m == "HEAD" {
			x.w.WriteHeader(AsAPIError(err).Status)
			return
		}
		writeStorageError(x.w, err)
	}
	if m == "OPTIONS" {
		x.app.tusOptions(x.w)
		x.w.WriteHeader(204)
		return
	}
	if v := x.r.Header.Get("Tus-Resumable"); v != "" && v != tusVersion {
		h.Set("Tus-Version", tusVersion)
		fail(storageErr(412, "InvalidRequest", "unsupported Tus-Resumable version "+v))
		return
	}
	if err := x.tusClaims(); err != nil {
		fail(err)
		return
	}
	id := strings.Trim(strings.TrimPrefix(sub, "/upload/resumable"), "/")
	var err error
	switch {
	case id == "" && m == "POST":
		err = x.tusCreate(ctx)
	case id == "":
		err = storageErr(405, "MethodNotAllowed", "method not allowed")
	case m == "HEAD":
		err = x.tusHead(id)
	case m == "PATCH":
		err = x.tusPatch(ctx, id)
	case m == "DELETE":
		err = x.tusTerminate(ctx, id)
	default:
		err = storageErr(405, "MethodNotAllowed", "method not allowed")
	}
	if err != nil {
		fail(err)
	}
}

func (x *storeReq) tusClaims() error {
	key := x.r.Header.Get("apikey")
	if key != "" {
		c, err := verifyJWT(x.secret, key)
		if err != nil || c["ref"] != x.slug {
			return storageErr(401, "Unauthorized", "Invalid API key")
		}
	}
	tok := strings.TrimSpace(strings.TrimPrefix(x.r.Header.Get("Authorization"), "Bearer "))
	if tok == "" {
		tok = key
	}
	if tok == "" {
		return storageErr(401, "Unauthorized", "authorization header or apikey is required")
	}
	claims, err := verifyJWT(x.secret, tok)
	if err != nil {
		return storageErr(403, "Unauthorized", err.Error())
	}
	x.claims = claims
	return nil
}

func (x *storeReq) uploadLimit(bi *bucketInfo) int64 {
	limit := int64(x.app.storageConfig().MaxFileSizeMB) << 20
	if bi.Limit.Valid && bi.Limit.Int64 < limit {
		limit = bi.Limit.Int64
	}
	return limit
}

var errTooLarge = storageErr(413, "Payload too large", "The object exceeded the maximum allowed size")

// parseTusMetadata decodes "key base64,key base64" (a key may have no value).
func parseTusMetadata(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, _ := strings.Cut(pair, " ")
		b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v))
		if err != nil {
			if b, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(strings.TrimSpace(v), "=")); err != nil {
				return nil, storageErr(400, "InvalidRequest", "Upload-Metadata: "+k+" is not base64")
			}
		}
		out[k] = string(b)
	}
	return out, nil
}

func (x *storeReq) tusCreate(ctx context.Context) error {
	hd := x.r.Header
	if hd.Get("Upload-Defer-Length") != "" {
		return storageErr(400, "InvalidRequest", "Upload-Defer-Length is not supported")
	}
	length, err := strconv.ParseInt(hd.Get("Upload-Length"), 10, 64)
	if err != nil || length < 0 {
		return storageErr(400, "InvalidRequest", "Upload-Length header is required")
	}
	md, err := parseTusMetadata(hd.Get("Upload-Metadata"))
	if err != nil {
		return err
	}
	bucket, name := md["bucketName"], md["objectName"]
	if bucket == "" {
		return storageErr(400, "InvalidRequest", "bucketName is required in Upload-Metadata")
	}
	if err := validObjectName(name); err != nil {
		return err
	}
	bi, err := x.app.bucketInfo(ctx, x.slug, bucket)
	if err != nil {
		return err
	}
	if length > x.uploadLimit(bi) {
		return errTooLarge
	}
	ct := md["contentType"]
	if ct != "" && !mimeAllowed(bi.Mimes, ct) {
		return storageErr(415, "invalid_mime_type", "mime type "+ct+" is not supported")
	}
	cc := strings.TrimSpace(md["cacheControl"])
	if _, err := strconv.Atoi(cc); err == nil {
		cc = "max-age=" + cc
	}
	if cc == "" {
		cc = "max-age=3600"
	}
	var userMeta map[string]any
	if s := md["metadata"]; s != "" {
		json.Unmarshal([]byte(s), &userMeta)
	}
	upsert := strings.EqualFold(hd.Get("x-upsert"), "true")
	if err := x.tusAuthorize(ctx, bucket, name, upsert, length, ct, cc); err != nil {
		return err
	}

	role, _ := x.claims["role"].(string)
	sub, _ := x.claims["sub"].(string)
	now := time.Now().UTC()
	version := randUUID()
	u := &tusUpload{ID: base64.RawURLEncoding.EncodeToString([]byte(bucket + "/" + name + "/" + version)), Version: version,
		Project: x.slug, Bucket: bucket, Name: name, Length: length, ContentType: ct, CacheControl: cc, UserMeta: userMeta,
		RawMeta: hd.Get("Upload-Metadata"), Upsert: upsert, Role: role, Sub: sub, Created: now, Expires: now.Add(tusExpiry)}
	if err := x.app.tusSave(u); err != nil {
		return err
	}
	// Relative to the request URL, so it is right whatever host or path prefix
	// (/proj/<slug> or a project subdomain behind a proxy) the client used.
	loc := u.ID
	if !strings.HasSuffix(x.r.URL.Path, "/") {
		loc = "resumable/" + u.ID
	}
	h := x.w.Header()
	h.Set("Location", loc)
	h.Set("Upload-Expires", u.Expires.Format(http.TimeFormat))
	offset := int64(0)
	if strings.HasPrefix(hd.Get("Content-Type"), "application/offset+octet-stream") || length == 0 { // creation-with-upload
		if offset, err = x.tusWrite(ctx, u, 0); err != nil {
			return err
		}
	}
	h.Set("Upload-Offset", strconv.FormatInt(offset, 10))
	x.w.WriteHeader(201)
	return nil
}

// tusAuthorize is a dry run of the final INSERT (or upsert) as the caller's
// role, so row level security refuses an upload before any byte is sent.
func (x *storeReq) tusAuthorize(ctx context.Context, bucket, name string, upsert bool, size int64, ct, cc string) error {
	errDry := errors.New("dry run")
	mj, _ := json.Marshal(map[string]any{"size": size, "mimetype": ct, "cacheControl": cc, "contentLength": size})
	err := x.app.asRole(ctx, x.slug, x.claims, func(tx *sql.Tx) error {
		q := `INSERT INTO storage.objects(bucket_id, name, owner, metadata) VALUES ($1,$2,$3,$4)`
		if upsert {
			q += ` ON CONFLICT (bucket_id, name) DO UPDATE SET metadata=EXCLUDED.metadata, owner=EXCLUDED.owner, updated_at=now()`
		}
		var id string
		if err := tx.QueryRowContext(ctx, q+` RETURNING id`, bucket, name, x.sub(), string(mj)).Scan(&id); err != nil {
			return err
		}
		return errDry
	})
	if err == errDry {
		return nil
	}
	return err
}

func (a *App) tusSave(u *tusUpload) error {
	if err := os.MkdirAll(a.tusDir(u.Project), 0o700); err != nil {
		return err
	}
	bin := a.tusFile(u.Project, u.Version, ".bin")
	f, err := os.OpenFile(bin, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	f.Close()
	b, _ := json.Marshal(u)
	tmp := a.tusFile(u.Project, u.Version, ".json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		os.Remove(bin)
		return err
	}
	return os.Rename(tmp, a.tusFile(u.Project, u.Version, ".json"))
}

func (a *App) tusRemove(u *tusUpload) {
	os.Remove(a.tusFile(u.Project, u.Version, ".json"))
	os.Remove(a.tusFile(u.Project, u.Version, ".bin"))
	tusLocks.Delete(u.Project + "/" + u.Version)
}

// tusLoad finds an upload of this project by its Location id. Ids name files
// only through the validated version uuid, never through raw user input.
func (a *App) tusLoad(slug, id string) (*tusUpload, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(id, "="))
	if err != nil {
		return nil, errUploadNotFound
	}
	s := string(raw)
	version := s[strings.LastIndex(s, "/")+1:]
	if !tusVersionRe.MatchString(version) {
		return nil, errUploadNotFound
	}
	b, err := os.ReadFile(a.tusFile(slug, version, ".json"))
	if err != nil {
		return nil, errUploadNotFound
	}
	u := &tusUpload{}
	if json.Unmarshal(b, u) != nil || u.ID != strings.TrimRight(id, "=") || u.Project != slug {
		return nil, errUploadNotFound
	}
	if time.Now().After(u.Expires) {
		a.tusRemove(u)
		return nil, storageErr(410, "not_found", "Upload expired")
	}
	return u, nil
}

// tusOwned loads an upload and checks the caller created it.
func (x *storeReq) tusOwned(id string) (*tusUpload, error) {
	u, err := x.app.tusLoad(x.slug, id)
	if err != nil {
		return nil, err
	}
	role, _ := x.claims["role"].(string)
	sub, _ := x.claims["sub"].(string)
	if role != u.Role || sub != u.Sub {
		return nil, storageErr(403, "Unauthorized", "this upload was created by another user")
	}
	return u, nil
}

func (x *storeReq) tusHead(id string) error {
	u, err := x.tusOwned(id)
	if err != nil {
		return err
	}
	st, err := os.Stat(x.app.tusFile(u.Project, u.Version, ".bin"))
	if err != nil {
		return errUploadNotFound
	}
	h := x.w.Header()
	h.Set("Upload-Offset", strconv.FormatInt(st.Size(), 10))
	h.Set("Upload-Length", strconv.FormatInt(u.Length, 10))
	h.Set("Upload-Expires", u.Expires.Format(http.TimeFormat))
	if u.RawMeta != "" {
		h.Set("Upload-Metadata", u.RawMeta)
	}
	x.w.WriteHeader(200)
	return nil
}

func (x *storeReq) tusPatch(ctx context.Context, id string) error {
	if !strings.HasPrefix(x.r.Header.Get("Content-Type"), "application/offset+octet-stream") {
		return storageErr(415, "InvalidRequest", "Content-Type must be application/offset+octet-stream")
	}
	offset, err := strconv.ParseInt(x.r.Header.Get("Upload-Offset"), 10, 64)
	if err != nil || offset < 0 {
		return storageErr(400, "InvalidRequest", "Upload-Offset header is required")
	}
	u, err := x.tusOwned(id)
	if err != nil {
		return err
	}
	n, err := x.tusWrite(ctx, u, offset)
	if err != nil {
		return err
	}
	x.w.Header().Set("Upload-Offset", strconv.FormatInt(n, 10))
	x.w.Header().Set("Upload-Expires", u.Expires.Format(http.TimeFormat))
	x.w.WriteHeader(204)
	return nil
}

func (x *storeReq) tusTerminate(ctx context.Context, id string) error {
	u, err := x.tusOwned(id)
	if err != nil {
		return err
	}
	unlock, err := tusLock(ctx, u.Project+"/"+u.Version)
	if err != nil {
		return err
	}
	defer unlock()
	x.app.tusRemove(u)
	x.w.WriteHeader(204)
	return nil
}

// tusLocks holds one slot per upload: a second PATCH waits for the first, then
// sees the new offset (409) instead of interleaving writes into the file.
var tusLocks sync.Map // "<project>/<version>" -> chan struct{}

func tusLock(ctx context.Context, key string) (func(), error) {
	v, _ := tusLocks.LoadOrStore(key, make(chan struct{}, 1))
	ch := v.(chan struct{})
	t := time.NewTimer(tusLockWait)
	defer t.Stop()
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-t.C:
		return nil, storageErr(423, "ResourceLocked", "another request is writing to this upload")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// tusWrite appends the request body at offset and finishes the upload when
// the last byte is in. The data file's size is the offset: bytes of an
// interrupted request stay, and the client resumes after them.
func (x *storeReq) tusWrite(ctx context.Context, u *tusUpload, offset int64) (int64, error) {
	unlock, err := tusLock(ctx, u.Project+"/"+u.Version)
	if err != nil {
		return 0, err
	}
	defer unlock()
	bin := x.app.tusFile(u.Project, u.Version, ".bin")
	if _, err := os.Stat(x.app.tusFile(u.Project, u.Version, ".json")); err != nil {
		return 0, errUploadNotFound // finished or terminated while we waited
	}
	// the bucket's rules may have changed since the upload was created
	bi, err := x.app.bucketInfo(ctx, x.slug, u.Bucket)
	if err != nil {
		x.app.tusRemove(u)
		return 0, err
	}
	if u.Length > x.uploadLimit(bi) {
		x.app.tusRemove(u)
		return 0, errTooLarge
	}
	f, err := os.OpenFile(bin, os.O_WRONLY, 0)
	if err != nil {
		return 0, errUploadNotFound
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return 0, err
	}
	size := st.Size()
	if offset != size {
		f.Close()
		return size, storageErr(409, "Conflict", fmt.Sprintf("Upload-Offset %d does not match the upload's offset %d", offset, size))
	}
	remaining := u.Length - size
	if x.r.ContentLength > remaining {
		f.Close()
		return size, storageErr(413, "Payload too large", "the chunk goes past Upload-Length")
	}
	if _, err := f.Seek(size, io.SeekStart); err != nil {
		f.Close()
		return size, err
	}
	n, werr := io.Copy(f, io.LimitReader(x.r.Body, remaining))
	if werr == nil && n == remaining {
		if k, _ := io.ReadAtLeast(x.r.Body, make([]byte, 1), 1); k > 0 {
			f.Truncate(size)
			f.Close()
			return size, storageErr(413, "Payload too large", "the chunk goes past Upload-Length")
		}
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return size + n, werr
	}
	if size+n < u.Length {
		return size + n, nil
	}
	return u.Length, x.tusFinish(ctx, u)
}

// tusFinish stores the assembled file as the object. Refusals (row level
// security, bucket rules) are final and drop the upload; other failures keep
// it, and a PATCH at the final offset with an empty body retries.
func (x *storeReq) tusFinish(ctx context.Context, u *tusUpload) error {
	bin := x.app.tusFile(u.Project, u.Version, ".bin")
	f, err := os.Open(bin)
	if err != nil {
		return errUploadNotFound
	}
	h := md5.New()
	head := make([]byte, 512)
	k, _ := io.ReadFull(f, head)
	h.Write(head[:k])
	_, err = io.Copy(h, f)
	f.Close()
	if err != nil {
		return err
	}
	ct := u.ContentType
	if ct == "" {
		ct = http.DetectContentType(head[:k])
	}
	err = func() error {
		bi, err := x.app.bucketInfo(ctx, x.slug, u.Bucket)
		if err != nil {
			return err
		}
		if !mimeAllowed(bi.Mimes, ct) {
			return storageErr(415, "invalid_mime_type", "mime type "+ct+" is not supported")
		}
		if u.Length > x.uploadLimit(bi) {
			return errTooLarge
		}
		sp := &spooled{path: bin, contentType: ct, cacheControl: u.CacheControl, size: u.Length, userMeta: u.UserMeta,
			etag: `"` + hex.EncodeToString(h.Sum(nil)) + `"`}
		_, err = x.storeObject(ctx, u.Bucket, u.Name, u.Upsert, "", sp)
		return err
	}()
	var pe *pq.Error
	if err == nil || errors.As(err, &pe) || AsAPIError(err).Status < 500 {
		x.app.tusRemove(u)
	}
	return err
}

// ---------------------------------------------------------------- expiry

// tusSweeper deletes expired uploads, and uploads of deleted projects.
func (a *App) tusSweeper(ctx context.Context) {
	exists := func(slug string) bool {
		_, err := a.project(slug)
		return err == nil || AsAPIError(err).Status != 404 // unsure (meta store down): keep
	}
	for {
		if n := a.tusSweep(time.Now(), exists); n > 0 {
			log.Printf("storage: removed %d expired resumable uploads", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Minute):
		}
	}
}

func (a *App) tusSweep(now time.Time, exists func(slug string) bool) int {
	root := a.Cfg.Path("tmp", "tus")
	projects, _ := os.ReadDir(root)
	removed := 0
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		dir := filepath.Join(root, p.Name())
		if !exists(p.Name()) {
			os.RemoveAll(dir)
			continue
		}
		files, _ := os.ReadDir(dir)
		for _, f := range files {
			name := f.Name()
			version, ext, _ := strings.Cut(name, ".")
			path := filepath.Join(dir, name)
			switch ext {
			case "json":
				var u tusUpload
				b, err := os.ReadFile(path)
				if err == nil && json.Unmarshal(b, &u) == nil && now.Before(u.Expires) {
					continue
				}
				// take the lock so a running PATCH finishes first
				if unlock, err := tusLock(context.Background(), p.Name()+"/"+version); err == nil {
					os.Remove(path)
					os.Remove(filepath.Join(dir, version+".bin"))
					tusLocks.Delete(p.Name() + "/" + version)
					unlock()
					removed++
				}
			default: // a data file without state, or a crashed state write
				if _, err := os.Stat(filepath.Join(dir, version+".json")); err == nil && ext == "bin" {
					continue
				}
				if info, err := f.Info(); err == nil && now.Sub(info.ModTime()) > time.Hour {
					os.Remove(path)
				}
			}
		}
	}
	return removed
}
