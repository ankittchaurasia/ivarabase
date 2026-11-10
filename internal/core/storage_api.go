package core

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
)

func jsonMarshal(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

func storageErr(status int, code, msg string) *APIError {
	return &APIError{Status: status, Code: code, Message: msg}
}

// writeStorageError uses the storage API's error shape (storage-js reads .message).
func writeStorageError(w http.ResponseWriter, err error) {
	var pe *pq.Error
	if errors.As(err, &pe) {
		switch {
		case strings.Contains(pe.Message, "row-level security"):
			err = storageErr(403, "Unauthorized", "new row violates row-level security policy")
		case pe.Code == "23505":
			err = storageErr(409, "Duplicate", "The resource already exists")
		case pe.Code == "23503":
			err = storageErr(404, "Bucket not found", "Bucket not found")
		default:
			err = storageErr(400, "InvalidRequest", pe.Message)
		}
	}
	ae := AsAPIError(err)
	writeJSON(w, ae.Status, map[string]any{"statusCode": strconv.Itoa(ae.Status), "error": ae.Code, "message": ae.Message})
}

var bucketIDRe = regexp.MustCompile(`^[A-Za-z0-9_.\-]{1,63}$`)

func validBucketID(id string) bool {
	return bucketIDRe.MatchString(id) && strings.Trim(id, ".") != ""
}

func validObjectName(n string) error {
	if n == "" || len(n) > 1024 || strings.HasPrefix(n, "/") || strings.Contains(n, "\x00") || strings.Contains(n, "\\") {
		return storageErr(400, "InvalidKey", "Invalid key: "+n)
	}
	for _, seg := range strings.Split(n, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return storageErr(400, "InvalidKey", "Invalid key: "+n)
		}
	}
	return nil
}

type storeReq struct {
	app    *App
	slug   string
	secret []byte
	claims map[string]any
	w      http.ResponseWriter
	r      *http.Request
	be     storageBackend
}

func (x *storeReq) ok(v any) { writeJSON(x.w, 200, v) }

// storagePublicPath reports routes that authorize by URL (public bucket or
// signed token) instead of an apikey.
func storagePublicPath(method, sub string) bool {
	return ((method == "GET" || method == "HEAD") && (strings.HasPrefix(sub, "/object/public/") || strings.HasPrefix(sub, "/object/sign/"))) ||
		(method == "PUT" && strings.HasPrefix(sub, "/object/upload/sign/")) ||
		tusPath(sub) // checks the bearer token itself: Supabase's docs send only authorization to the resumable endpoint
}

// serveStorage handles /proj/<slug>/storage/v1/<sub>.
func (a *App) serveStorage(w http.ResponseWriter, r *http.Request, slug, sub string, secret []byte) {
	if err := a.ensureProjectAPIOnce(slug); err != nil {
		writeStorageError(w, err)
		return
	}
	x := &storeReq{app: a, slug: slug, secret: secret, w: w, r: r, be: a.storageBackend()}
	m := r.Method
	ctx := r.Context()

	// URL-authorized routes first
	switch {
	case (m == "GET" || m == "HEAD") && strings.HasPrefix(sub, "/object/public/"):
		x.servePublic(ctx, strings.TrimPrefix(sub, "/object/public/"))
		return
	case (m == "GET" || m == "HEAD") && strings.HasPrefix(sub, "/object/sign/"):
		x.serveSigned(ctx, strings.TrimPrefix(sub, "/object/sign/"))
		return
	case m == "PUT" && strings.HasPrefix(sub, "/object/upload/sign/"):
		x.uploadSigned(ctx, strings.TrimPrefix(sub, "/object/upload/sign/"))
		return
	case tusPath(sub):
		x.serveTUS(ctx, sub)
		return
	}

	tok := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	claims, err := verifyJWT(secret, tok)
	if err != nil {
		writeStorageError(w, storageErr(403, "Unauthorized", err.Error()))
		return
	}
	x.claims = claims

	var res any
	switch {
	case sub == "/bucket" && m == "GET":
		res, err = x.listBuckets(ctx, "")
	case sub == "/bucket" && m == "POST":
		res, err = x.createBucket(ctx)
	case strings.HasPrefix(sub, "/bucket/") && strings.HasSuffix(sub, "/empty") && m == "POST":
		res, err = x.emptyBucket(ctx, strings.TrimSuffix(strings.TrimPrefix(sub, "/bucket/"), "/empty"))
	case strings.HasPrefix(sub, "/bucket/"):
		id := strings.TrimPrefix(sub, "/bucket/")
		switch m {
		case "GET":
			var list []map[string]any
			list, err = x.listBuckets(ctx, id)
			if err == nil {
				if len(list) == 0 {
					err = storageErr(404, "Bucket not found", "Bucket not found")
				} else {
					res = list[0]
				}
			}
		case "PUT":
			res, err = x.updateBucket(ctx, id)
		case "DELETE":
			res, err = x.deleteBucket(ctx, id)
		default:
			err = storageErr(405, "MethodNotAllowed", "method not allowed")
		}
	case strings.HasPrefix(sub, "/object/list/") && m == "POST":
		res, err = x.list(ctx, strings.TrimPrefix(sub, "/object/list/"))
	case sub == "/object/move" && m == "POST":
		res, err = x.move(ctx, false)
	case sub == "/object/copy" && m == "POST":
		res, err = x.move(ctx, true)
	case strings.HasPrefix(sub, "/object/upload/sign/") && m == "POST":
		res, err = x.createUploadURL(ctx, strings.TrimPrefix(sub, "/object/upload/sign/"))
	case strings.HasPrefix(sub, "/object/sign/") && m == "POST":
		res, err = x.sign(ctx, strings.TrimPrefix(sub, "/object/sign/"))
	case strings.HasPrefix(sub, "/object/info/") && m == "GET":
		res, err = x.info(ctx, strings.TrimPrefix(strings.TrimPrefix(sub, "/object/info/"), "authenticated/"))
	case strings.HasPrefix(sub, "/object/authenticated/") && (m == "GET" || m == "HEAD"):
		x.download(ctx, strings.TrimPrefix(sub, "/object/authenticated/"))
		return
	case strings.HasPrefix(sub, "/object/"):
		rest := strings.TrimPrefix(sub, "/object/")
		bucket, name, _ := strings.Cut(rest, "/")
		switch {
		case m == "DELETE" && name == "":
			res, err = x.remove(ctx, bucket, nil)
		case m == "DELETE":
			res, err = x.remove(ctx, bucket, []string{name})
		case m == "POST":
			res, err = x.upload(ctx, bucket, name, strings.EqualFold(r.Header.Get("x-upsert"), "true"), "")
		case m == "PUT":
			res, err = x.upload(ctx, bucket, name, true, "")
		case m == "GET" || m == "HEAD":
			x.download(ctx, rest)
			return
		default:
			err = storageErr(405, "MethodNotAllowed", "method not allowed")
		}
	default:
		err = storageErr(404, "NotFound", "unknown storage endpoint "+m+" "+sub)
	}
	if err != nil {
		writeStorageError(w, err)
		return
	}
	x.ok(res)
}

func (x *storeReq) sub() any {
	if s, ok := x.claims["sub"].(string); ok && uuidLike(s) {
		return s
	}
	return nil
}

func (x *storeReq) readBody() (map[string]any, error) {
	b, err := io.ReadAll(io.LimitReader(x.r.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	m := map[string]any{}
	if len(strings.TrimSpace(string(b))) > 0 {
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, storageErr(400, "InvalidJSON", "request body must be JSON")
		}
	}
	return m, nil
}

// ---------------------------------------------------------------- buckets

func scanBucket(r rowScanner) (map[string]any, error) {
	var id, name string
	var owner sql.NullString
	var public bool
	var limit sql.NullInt64
	var mimes []string
	var c, u time.Time
	if err := r.Scan(&id, &name, &owner, &public, &limit, pq.Array(&mimes), &c, &u); err != nil {
		return nil, err
	}
	m := map[string]any{"id": id, "name": name, "owner": owner.String, "public": public, "created_at": c, "updated_at": u,
		"file_size_limit": nil, "allowed_mime_types": nil}
	if limit.Valid {
		m["file_size_limit"] = limit.Int64
	}
	if mimes != nil {
		m["allowed_mime_types"] = mimes
	}
	return m, nil
}

const bucketCols = `id, name, owner, public, file_size_limit, allowed_mime_types, created_at, updated_at`

func (x *storeReq) listBuckets(ctx context.Context, id string) ([]map[string]any, error) {
	out := []map[string]any{}
	err := x.app.asRole(ctx, x.slug, x.claims, func(tx *sql.Tx) error {
		q := `SELECT ` + bucketCols + ` FROM storage.buckets`
		args := []any{}
		if id != "" {
			q += ` WHERE id=$1`
			args = append(args, id)
		}
		rows, err := tx.QueryContext(ctx, q+` ORDER BY name`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			b, err := scanBucket(rows)
			if err != nil {
				return err
			}
			out = append(out, b)
		}
		return rows.Err()
	})
	return out, err
}

func parseSize(v any) (any, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case float64:
		if t <= 0 {
			return nil, nil
		}
		return int64(t), nil
	case string:
		s := strings.ToUpper(strings.TrimSpace(t))
		if s == "" {
			return nil, nil
		}
		mult := int64(1)
		for _, u := range []struct {
			suf string
			m   int64
		}{{"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"B", 1}} {
			if strings.HasSuffix(s, u.suf) {
				mult, s = u.m, strings.TrimSpace(strings.TrimSuffix(s, u.suf))
				break
			}
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || f <= 0 {
			return nil, storageErr(400, "InvalidRequest", "invalid file_size_limit")
		}
		return int64(f * float64(mult)), nil
	}
	return nil, storageErr(400, "InvalidRequest", "invalid file_size_limit")
}

func strArray(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := []string{}
	for _, e := range arr {
		if s, ok := e.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (x *storeReq) createBucket(ctx context.Context) (any, error) {
	body, err := x.readBody()
	if err != nil {
		return nil, err
	}
	id := str(body, "id")
	if id == "" {
		id = str(body, "name")
	}
	if !validBucketID(id) {
		return nil, storageErr(400, "InvalidBucketName", "Bucket name invalid: use letters, numbers, dots, hyphens and underscores")
	}
	limit, err := parseSize(body["file_size_limit"])
	if err != nil {
		return nil, err
	}
	public, _ := body["public"].(bool)
	mimes := strArray(body["allowed_mime_types"])
	return map[string]any{"name": id}, x.app.asRole(ctx, x.slug, x.claims, func(tx *sql.Tx) error {
		var m any
		if mimes != nil {
			m = pq.Array(mimes)
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO storage.buckets(id, name, owner, public, file_size_limit, allowed_mime_types) VALUES ($1,$1,$2,$3,$4,$5)`,
			id, x.sub(), public, limit, m)
		return err
	})
}

func (x *storeReq) updateBucket(ctx context.Context, id string) (any, error) {
	body, err := x.readBody()
	if err != nil {
		return nil, err
	}
	sets, args := []string{}, []any{id}
	if v, ok := body["public"].(bool); ok {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("public=$%d", len(args)))
	}
	if v, ok := body["file_size_limit"]; ok {
		limit, err := parseSize(v)
		if err != nil {
			return nil, err
		}
		args = append(args, limit)
		sets = append(sets, fmt.Sprintf("file_size_limit=$%d", len(args)))
	}
	if v, ok := body["allowed_mime_types"]; ok {
		var m any
		if s := strArray(v); s != nil {
			m = pq.Array(s)
		}
		args = append(args, m)
		sets = append(sets, fmt.Sprintf("allowed_mime_types=$%d", len(args)))
	}
	if len(sets) == 0 {
		return map[string]any{"message": "Successfully updated"}, nil
	}
	err = x.app.asRole(ctx, x.slug, x.claims, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE storage.buckets SET `+strings.Join(sets, ", ")+`, updated_at=now() WHERE id=$1`, args...)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return storageErr(404, "Bucket not found", "Bucket not found")
		}
		return nil
	})
	return map[string]any{"message": "Successfully updated"}, err
}

func (x *storeReq) emptyBucket(ctx context.Context, id string) (any, error) {
	if _, err := x.remove(ctx, id, nil); err != nil {
		return nil, err
	}
	return map[string]any{"message": "Successfully emptied"}, nil
}

func (x *storeReq) deleteBucket(ctx context.Context, id string) (any, error) {
	err := x.app.asRole(ctx, x.slug, x.claims, func(tx *sql.Tx) error {
		var n int
		tx.QueryRowContext(ctx, `SELECT count(*) FROM storage.objects WHERE bucket_id=$1`, id).Scan(&n)
		if n > 0 {
			return storageErr(409, "InvalidRequest", "The bucket you tried to delete is not empty")
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM storage.buckets WHERE id=$1`, id)
		if err != nil {
			return err
		}
		if c, _ := res.RowsAffected(); c == 0 {
			return storageErr(404, "Bucket not found", "Bucket not found")
		}
		return nil
	})
	return map[string]any{"message": "Successfully deleted"}, err
}

// ---------------------------------------------------------------- upload

type bucketInfo struct {
	Public bool
	Limit  sql.NullInt64
	Mimes  []string
}

func (a *App) bucketInfo(ctx context.Context, slug, id string) (*bucketInfo, error) {
	db, err := a.projectDB(slug)
	if err != nil {
		return nil, err
	}
	b := &bucketInfo{}
	err = db.QueryRowContext(ctx, `SELECT public, file_size_limit, allowed_mime_types FROM storage.buckets WHERE id=$1`, id).Scan(&b.Public, &b.Limit, pq.Array(&b.Mimes))
	if err != nil {
		return nil, storageErr(404, "Bucket not found", "Bucket not found")
	}
	return b, nil
}

func mimeAllowed(allowed []string, ct string) bool {
	if len(allowed) == 0 {
		return true
	}
	base, _, _ := mime.ParseMediaType(ct)
	for _, a := range allowed {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == base || a == "*/*" || (strings.HasSuffix(a, "/*") && strings.HasPrefix(base, strings.TrimSuffix(a, "*"))) {
			return true
		}
	}
	return false
}

type spooled struct {
	path, contentType, cacheControl, etag string
	size                                  int64
	userMeta                              map[string]any
}

// spool reads the upload (raw body or multipart form, as storage-js sends it)
// into a temp file, enforcing the size limit, and computes size and ETag.
func (x *storeReq) spool(limit int64) (*spooled, error) {
	s := &spooled{contentType: x.r.Header.Get("Content-Type"), cacheControl: x.r.Header.Get("Cache-Control")}
	if s.cacheControl == "" {
		s.cacheControl = "max-age=3600"
	}
	body := http.MaxBytesReader(x.w, x.r.Body, limit+1<<20)
	var src io.Reader = body
	if mt, params, _ := mime.ParseMediaType(s.contentType); mt == "multipart/form-data" {
		mr := multipart.NewReader(body, params["boundary"])
		var filePart *multipart.Part
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, storageErr(400, "InvalidRequest", "bad multipart body: "+err.Error())
			}
			switch p.FormName() {
			case "cacheControl":
				v, _ := io.ReadAll(io.LimitReader(p, 256))
				if cc := strings.TrimSpace(string(v)); cc != "" {
					if _, err := strconv.Atoi(cc); err == nil {
						cc = "max-age=" + cc
					}
					s.cacheControl = cc
				}
				continue
			case "metadata":
				v, _ := io.ReadAll(io.LimitReader(p, 64<<10))
				json.Unmarshal(v, &s.userMeta)
				continue
			}
			filePart = p
			break
		}
		if filePart == nil {
			return nil, storageErr(400, "InvalidRequest", "no file in multipart body")
		}
		s.contentType = filePart.Header.Get("Content-Type")
		src = filePart
	} else if cc := x.r.Header.Get("Cache-Control"); cc != "" {
		if _, err := strconv.Atoi(cc); err == nil {
			s.cacheControl = "max-age=" + cc
		}
	}
	f, err := os.CreateTemp(x.app.Cfg.Path("tmp"), "up-")
	if err != nil {
		return nil, err
	}
	s.path = f.Name()
	h := md5.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(src, limit+1))
	f.Close()
	if err != nil {
		os.Remove(s.path)
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, storageErr(413, "Payload too large", "The object exceeded the maximum allowed size")
		}
		return nil, err
	}
	if n > limit {
		os.Remove(s.path)
		return nil, storageErr(413, "Payload too large", "The object exceeded the maximum allowed size")
	}
	s.size = n
	s.etag = `"` + hex.EncodeToString(h.Sum(nil)) + `"`
	if s.contentType == "" || strings.HasPrefix(s.contentType, "multipart/") {
		if f, err := os.Open(s.path); err == nil {
			buf := make([]byte, 512)
			k, _ := f.Read(buf)
			f.Close()
			s.contentType = http.DetectContentType(buf[:k])
		}
	}
	return s, nil
}

// upload stores an object. The metadata row is written as the caller's role
// first (row level security decides), then the bytes, then the transaction
// commits - a refused or failed upload leaves nothing behind.
func (x *storeReq) upload(ctx context.Context, bucket, name string, upsert bool, ownerOverride string) (any, error) {
	if err := validObjectName(name); err != nil {
		return nil, err
	}
	bi, err := x.app.bucketInfo(ctx, x.slug, bucket)
	if err != nil {
		return nil, err
	}
	limit := int64(x.app.storageConfig().MaxFileSizeMB) << 20
	if bi.Limit.Valid && bi.Limit.Int64 < limit {
		limit = bi.Limit.Int64
	}
	sp, err := x.spool(limit)
	if err != nil {
		return nil, err
	}
	defer os.Remove(sp.path)
	if !mimeAllowed(bi.Mimes, sp.contentType) {
		return nil, storageErr(415, "invalid_mime_type", "mime type "+sp.contentType+" is not supported")
	}
	return x.storeObject(ctx, bucket, name, upsert, ownerOverride, sp)
}

// storeObject writes the metadata row and the bytes of a spooled file (shared
// by plain and resumable uploads, so both authorize exactly the same way).
func (x *storeReq) storeObject(ctx context.Context, bucket, name string, upsert bool, ownerOverride string, sp *spooled) (any, error) {
	now := time.Now().UTC()
	meta := map[string]any{"size": sp.size, "mimetype": sp.contentType, "cacheControl": sp.cacheControl, "eTag": sp.etag,
		"lastModified": now, "contentLength": sp.size, "httpStatusCode": 200}
	mj, _ := json.Marshal(meta)
	var owner any = x.sub()
	if ownerOverride != "" {
		owner = ownerOverride
	}
	var id string
	err := x.app.asRole(ctx, x.slug, x.claims, func(tx *sql.Tx) error {
		q := `INSERT INTO storage.objects(bucket_id, name, owner, metadata) VALUES ($1,$2,$3,$4)`
		if upsert {
			q += ` ON CONFLICT (bucket_id, name) DO UPDATE SET metadata=EXCLUDED.metadata, owner=EXCLUDED.owner, updated_at=now()`
		}
		if err := tx.QueryRowContext(ctx, q+` RETURNING id`, bucket, name, owner, string(mj)).Scan(&id); err != nil {
			return err
		}
		f, err := os.Open(sp.path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = x.be.put(ctx, x.slug, bucket, id, f, sp.size)
		return err
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"Key": bucket + "/" + name, "Id": id}, nil
}

// ---------------------------------------------------------------- download

type objRow struct {
	ID      string
	Bucket  string
	Name    string
	Meta    map[string]any
	Created time.Time
	Updated time.Time
	Owner   sql.NullString
}

func scanObj(r rowScanner) (*objRow, error) {
	o := &objRow{}
	var meta []byte
	if err := r.Scan(&o.ID, &o.Bucket, &o.Name, &meta, &o.Created, &o.Updated, &o.Owner); err != nil {
		return nil, err
	}
	json.Unmarshal(meta, &o.Meta)
	return o, nil
}

const objCols = `id, bucket_id, name, coalesce(metadata,'{}'), created_at, updated_at, owner`

var errObjNotFound = storageErr(404, "not_found", "Object not found")

func (x *storeReq) findAsRole(ctx context.Context, bucket, name string) (*objRow, error) {
	var o *objRow
	err := x.app.asRole(ctx, x.slug, x.claims, func(tx *sql.Tx) error {
		var err error
		o, err = scanObj(tx.QueryRowContext(ctx, `SELECT `+objCols+` FROM storage.objects WHERE bucket_id=$1 AND name=$2`, bucket, name))
		if errors.Is(err, sql.ErrNoRows) {
			return errObjNotFound
		}
		return err
	})
	return o, err
}

func (a *App) findObject(ctx context.Context, slug, bucket, name string) (*objRow, error) {
	db, err := a.projectDB(slug)
	if err != nil {
		return nil, err
	}
	o, err := scanObj(db.QueryRowContext(ctx, `SELECT `+objCols+` FROM storage.objects WHERE bucket_id=$1 AND name=$2`, bucket, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errObjNotFound
	}
	return o, err
}

func (x *storeReq) sendObject(o *objRow) {
	ct, _ := o.Meta["mimetype"].(string)
	etag, _ := o.Meta["eTag"].(string)
	if cc, _ := o.Meta["cacheControl"].(string); cc != "" {
		x.w.Header().Set("Cache-Control", cc)
	}
	if d, ok := x.r.URL.Query()["download"]; ok {
		fname := d[0]
		if fname == "" {
			fname = o.Name[strings.LastIndex(o.Name, "/")+1:]
		}
		x.w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": fname}))
	}
	x.w.Header().Set("X-Content-Type-Options", "nosniff")
	// Uploaded files are untrusted: an HTML or SVG file in a public bucket must
	// not run script on our origin (the panel's, for /proj/<slug>/storage URLs).
	// Images, video, PDFs and downloads are unaffected.
	x.w.Header().Set("Content-Security-Policy", "sandbox")
	if err := x.be.serve(x.w, x.r, x.slug, o.Bucket, o.ID, ct, etag, o.Updated); err != nil {
		writeStorageError(x.w, storageErr(404, "not_found", "Object data not found: "+err.Error()))
	}
}

func (x *storeReq) download(ctx context.Context, path string) {
	bucket, name, _ := strings.Cut(path, "/")
	o, err := x.findAsRole(ctx, bucket, name)
	if err != nil {
		writeStorageError(x.w, err)
		return
	}
	x.sendObject(o)
}

func (x *storeReq) servePublic(ctx context.Context, path string) {
	bucket, name, _ := strings.Cut(path, "/")
	bi, err := x.app.bucketInfo(ctx, x.slug, bucket)
	if err != nil || !bi.Public {
		writeStorageError(x.w, storageErr(400, "not_found", "Bucket not found or not public"))
		return
	}
	o, err := x.app.findObject(ctx, x.slug, bucket, name)
	if err != nil {
		writeStorageError(x.w, err)
		return
	}
	x.sendObject(o)
}

func (x *storeReq) info(ctx context.Context, path string) (any, error) {
	bucket, name, _ := strings.Cut(path, "/")
	o, err := x.findAsRole(ctx, bucket, name)
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": o.ID, "name": o.Name, "bucket_id": o.Bucket, "version": o.ID, "size": o.Meta["size"],
		"content_type": o.Meta["mimetype"], "cache_control": o.Meta["cacheControl"], "etag": o.Meta["eTag"], "metadata": o.Meta,
		"last_modified": o.Updated, "created_at": o.Created}, nil
}

// ---------------------------------------------------------------- list / delete / move

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func (x *storeReq) list(ctx context.Context, bucket string) (any, error) {
	body, err := x.readBody()
	if err != nil {
		return nil, err
	}
	return x.app.listObjects(ctx, x.slug, x.claims, bucket, body)
}

// listObjects returns the files and folders directly under a prefix.
func (a *App) listObjects(ctx context.Context, slug string, claims map[string]any, bucket string, body map[string]any) ([]map[string]any, error) {
	prefix := strings.TrimLeft(str(body, "prefix"), "/")
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	limit, offset := 100, 0
	if v, ok := body["limit"].(float64); ok && v > 0 {
		limit = int(v)
	}
	if v, ok := body["offset"].(float64); ok && v > 0 {
		offset = int(v)
	}
	search := strings.ToLower(str(body, "search"))
	sortCol, desc := "name", false
	if sb := obj(body, "sortBy"); sb != nil {
		if c := str(sb, "column"); c == "created_at" || c == "updated_at" || c == "last_accessed_at" {
			sortCol = c
		}
		desc = strings.EqualFold(str(sb, "order"), "desc")
	}
	var objs []*objRow
	err := a.asRole(ctx, slug, claims, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT `+objCols+` FROM storage.objects WHERE bucket_id=$1 AND name LIKE $2 ESCAPE '\' ORDER BY name LIMIT 20000`,
			bucket, likeEscape(prefix)+"%")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			o, err := scanObj(rows)
			if err != nil {
				return err
			}
			objs = append(objs, o)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	folders := map[string]bool{}
	var folderNames []string
	var files []*objRow
	for _, o := range objs {
		rest := strings.TrimPrefix(o.Name, prefix)
		if i := strings.Index(rest, "/"); i >= 0 {
			f := rest[:i]
			if !folders[f] && strings.HasPrefix(strings.ToLower(f), search) {
				folders[f] = true
				folderNames = append(folderNames, f)
			}
			continue
		}
		if strings.HasPrefix(strings.ToLower(rest), search) {
			files = append(files, o)
		}
	}
	sort.Strings(folderNames)
	sort.SliceStable(files, func(i, j int) bool {
		var less bool
		switch sortCol {
		case "created_at":
			less = files[i].Created.Before(files[j].Created)
		case "updated_at", "last_accessed_at":
			less = files[i].Updated.Before(files[j].Updated)
		default:
			less = files[i].Name < files[j].Name
		}
		if desc {
			return !less
		}
		return less
	})
	out := []map[string]any{}
	for _, f := range folderNames {
		out = append(out, map[string]any{"name": f, "id": nil, "updated_at": nil, "created_at": nil, "last_accessed_at": nil, "metadata": nil})
	}
	for _, o := range files {
		out = append(out, map[string]any{"name": strings.TrimPrefix(o.Name, prefix), "id": o.ID, "updated_at": o.Updated, "created_at": o.Created,
			"last_accessed_at": o.Updated, "metadata": o.Meta})
	}
	if offset >= len(out) {
		return []map[string]any{}, nil
	}
	end := min(len(out), offset+limit)
	return out[offset:end], nil
}

// remove deletes objects by name (or everything in the bucket when names is
// nil) as the caller's role, then deletes the bytes of what was removed.
func (x *storeReq) remove(ctx context.Context, bucket string, names []string) (any, error) {
	if names == nil && x.r.Method == "DELETE" {
		body, err := x.readBody()
		if err != nil {
			return nil, err
		}
		names = strArray(body["prefixes"])
		if names == nil {
			return nil, storageErr(400, "InvalidRequest", "prefixes is required")
		}
	}
	return x.app.removeObjects(ctx, x.slug, x.claims, x.be, bucket, names)
}

func (a *App) removeObjects(ctx context.Context, slug string, claims map[string]any, be storageBackend, bucket string, names []string) ([]map[string]any, error) {
	var removed []*objRow
	err := a.asRole(ctx, slug, claims, func(tx *sql.Tx) error {
		q := `DELETE FROM storage.objects WHERE bucket_id=$1`
		args := []any{bucket}
		if names != nil {
			q += ` AND name = ANY($2)`
			args = append(args, pq.Array(names))
		}
		rows, err := tx.QueryContext(ctx, q+` RETURNING `+objCols, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			o, err := scanObj(rows)
			if err != nil {
				return err
			}
			removed = append(removed, o)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, o := range removed {
		be.remove(ctx, slug, o.Bucket, o.ID)
		out = append(out, map[string]any{"id": o.ID, "name": o.Name, "bucket_id": o.Bucket, "metadata": o.Meta,
			"created_at": o.Created, "updated_at": o.Updated, "owner": o.Owner.String})
	}
	return out, nil
}

func (x *storeReq) move(ctx context.Context, copy bool) (any, error) {
	body, err := x.readBody()
	if err != nil {
		return nil, err
	}
	src, dst := str(body, "sourceKey"), str(body, "destinationKey")
	bucket := str(body, "bucketId")
	dstBucket := str(body, "destinationBucket")
	if dstBucket == "" {
		dstBucket = bucket
	}
	return x.app.moveObject(ctx, x.slug, x.claims, x.be, bucket, src, dstBucket, dst, copy)
}

func (a *App) moveObject(ctx context.Context, slug string, claims map[string]any, be storageBackend, bucket, src, dstBucket, dst string, copy bool) (any, error) {
	if err := validObjectName(src); err != nil {
		return nil, err
	}
	if err := validObjectName(dst); err != nil {
		return nil, err
	}
	sub, _ := claims["sub"].(string)
	var owner any
	if uuidLike(sub) {
		owner = sub
	}
	var srcID, newID string
	err := a.asRole(ctx, slug, claims, func(tx *sql.Tx) error {
		if copy {
			err := tx.QueryRowContext(ctx, `INSERT INTO storage.objects(bucket_id, name, owner, metadata)
				SELECT $3, $4, $5, metadata FROM storage.objects WHERE bucket_id=$1 AND name=$2 RETURNING id,
				(SELECT id FROM storage.objects WHERE bucket_id=$1 AND name=$2)`, bucket, src, dstBucket, dst, owner).Scan(&newID, &srcID)
			if errors.Is(err, sql.ErrNoRows) {
				return errObjNotFound
			}
			if err != nil {
				return err
			}
			return be.copy(ctx, slug, bucket, srcID, newID)
		}
		err := tx.QueryRowContext(ctx, `UPDATE storage.objects SET bucket_id=$3, name=$4, updated_at=now() WHERE bucket_id=$1 AND name=$2 RETURNING id`,
			bucket, src, dstBucket, dst).Scan(&srcID)
		if errors.Is(err, sql.ErrNoRows) {
			return errObjNotFound
		}
		if err != nil {
			return err
		}
		if dstBucket != bucket { // bytes are stored per bucket: copy them before the row moves
			rc, err := be.open(ctx, slug, bucket, srcID)
			if err != nil {
				return err
			}
			defer rc.Close()
			if _, err := be.put(ctx, slug, dstBucket, srcID, rc, -1); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !copy && dstBucket != bucket {
		be.remove(ctx, slug, bucket, srcID)
	}
	if copy {
		return map[string]any{"Key": dstBucket + "/" + dst, "Id": newID}, nil
	}
	return map[string]any{"message": "Successfully moved"}, nil
}

// ---------------------------------------------------------------- signed URLs

func (x *storeReq) sign(ctx context.Context, path string) (any, error) {
	body, err := x.readBody()
	if err != nil {
		return nil, err
	}
	exp := 3600
	if v, ok := body["expiresIn"].(float64); ok && v > 0 {
		exp = int(v)
	}
	bucket, name, _ := strings.Cut(path, "/")
	if name == "" { // batch: POST /object/sign/{bucket} {paths}
		out := []map[string]any{}
		for _, p := range strArray(body["paths"]) {
			item := map[string]any{"path": p, "error": nil, "signedURL": nil}
			if _, err := x.findAsRole(ctx, bucket, p); err != nil {
				item["error"] = "Either the object does not exist or you do not have access to it"
			} else {
				item["signedURL"] = x.app.signedPath(x.slug, x.secret, bucket, p, exp)
			}
			out = append(out, item)
		}
		return out, nil
	}
	if _, err := x.findAsRole(ctx, bucket, name); err != nil {
		return nil, err
	}
	return map[string]any{"signedURL": x.app.signedPath(x.slug, x.secret, bucket, name, exp)}, nil
}

func (a *App) signedPath(slug string, secret []byte, bucket, name string, exp int) string {
	tok := signJWT(secret, map[string]any{"typ": "storage_sign", "url": bucket + "/" + name, "iat": time.Now().Unix(),
		"exp": time.Now().Add(time.Duration(exp) * time.Second).Unix()})
	return "/object/sign/" + bucket + "/" + urlPathEscape(name) + "?token=" + tok
}

func urlPathEscape(name string) string {
	parts := strings.Split(name, "/")
	for i, p := range parts {
		parts[i] = strings.ReplaceAll(pathEscape(p), "+", "%20")
	}
	return strings.Join(parts, "/")
}

func (x *storeReq) serveSigned(ctx context.Context, path string) {
	c, err := verifyJWT(x.secret, x.r.URL.Query().Get("token"))
	if err != nil || c["typ"] != "storage_sign" || c["url"] != path {
		writeStorageError(x.w, storageErr(400, "InvalidSignature", "The signature is invalid or has expired"))
		return
	}
	bucket, name, _ := strings.Cut(path, "/")
	o, err := x.app.findObject(ctx, x.slug, bucket, name)
	if err != nil {
		writeStorageError(x.w, err)
		return
	}
	x.sendObject(o)
}

func (x *storeReq) createUploadURL(ctx context.Context, path string) (any, error) {
	bucket, name, _ := strings.Cut(path, "/")
	if err := validObjectName(name); err != nil {
		return nil, err
	}
	upsert := strings.EqualFold(x.r.Header.Get("x-upsert"), "true")
	// dry run: would the caller be allowed to insert this object?
	errDry := errors.New("dry run")
	err := x.app.asRole(ctx, x.slug, x.claims, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO storage.objects(bucket_id, name, owner) VALUES ($1,$2,$3)`, bucket, name, x.sub()); err != nil {
			if upsert && strings.Contains(err.Error(), "duplicate key") {
				return errDry
			}
			return err
		}
		return errDry
	})
	if err != nil && err != errDry {
		return nil, err
	}
	owner, _ := x.sub().(string)
	tok := signJWT(x.secret, map[string]any{"typ": "storage_upload", "url": bucket + "/" + name, "owner": owner, "upsert": upsert,
		"iat": time.Now().Unix(), "exp": time.Now().Add(2 * time.Hour).Unix()})
	return map[string]any{"url": "/object/upload/sign/" + bucket + "/" + urlPathEscape(name) + "?token=" + tok, "token": tok}, nil
}

func (x *storeReq) uploadSigned(ctx context.Context, path string) {
	c, err := verifyJWT(x.secret, x.r.URL.Query().Get("token"))
	if err != nil || c["typ"] != "storage_upload" || c["url"] != path {
		writeStorageError(x.w, storageErr(400, "InvalidSignature", "The signature is invalid or has expired"))
		return
	}
	bucket, name, _ := strings.Cut(path, "/")
	owner, _ := c["owner"].(string)
	upsert, _ := c["upsert"].(bool)
	x.claims = map[string]any{"role": roleService}
	res, err := x.upload(ctx, bucket, name, upsert, owner)
	if err != nil {
		writeStorageError(x.w, err)
		return
	}
	x.ok(res)
}

func pathEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
