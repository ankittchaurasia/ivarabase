package core

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/lib/pq"
)

// Storage management for the UI, CLI and MCP. These run with the
// service_role (row level security bypassed), like Supabase's dashboard.

var serviceClaims = map[string]any{"role": roleService}

func registerStorageOps(r *Registry) {
	projectParam := Param{Name: "project", Type: "string", Required: true, Desc: "project slug"}
	bucketParam := Param{Name: "bucket", Type: "string", Required: true}
	ready := func(c *Ctx, a Args) error { return c.App.ensureProjectAPIOnce(a.Str("project")) }

	r.add(&Op{Name: "storage.buckets_list", Tag: "storage", Method: "GET", Path: "/api/v1/projects/{project}/storage/buckets", Level: LevelRead,
		Summary: "Buckets with file counts and total size",
		Params:  []Param{projectParam},
		Run: func(c *Ctx, a Args) (any, error) {
			if err := ready(c, a); err != nil {
				return nil, err
			}
			db, err := c.App.projectDB(a.Str("project"))
			if err != nil {
				return nil, err
			}
			rows, err := db.QueryContext(c, `SELECT b.id, b.name, b.owner, b.public, b.file_size_limit, b.allowed_mime_types, b.created_at, b.updated_at,
				count(o.id), coalesce(sum((o.metadata->>'size')::bigint),0)
				FROM storage.buckets b LEFT JOIN storage.objects o ON o.bucket_id=b.id GROUP BY b.id ORDER BY b.name`)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			out := []map[string]any{}
			for rows.Next() {
				var id, name string
				var owner sql.NullString
				var public bool
				var limit sql.NullInt64
				var mimes []string
				var cr, up time.Time
				var n, size int64
				if err := rows.Scan(&id, &name, &owner, &public, &limit, pq.Array(&mimes), &cr, &up, &n, &size); err != nil {
					return nil, err
				}
				b := map[string]any{"id": id, "name": name, "public": public, "created_at": cr, "updated_at": up, "objects": n, "size_bytes": size,
					"file_size_limit": nil, "allowed_mime_types": mimes}
				if limit.Valid {
					b["file_size_limit"] = limit.Int64
				}
				out = append(out, b)
			}
			return out, rows.Err()
		}})

	bucketFields := []Param{
		{Name: "public", Type: "boolean", Desc: "public buckets serve files at /object/public/... without a key"},
		{Name: "file_size_limit", Type: "string", Desc: "e.g. 5MB, 1GB or a byte count; empty = server limit"},
		{Name: "allowed_mime_types", Type: "array", Desc: "e.g. image/*, application/pdf; empty = any"},
	}
	setBucket := func(c *Ctx, a Args, create bool) (any, error) {
		if err := ready(c, a); err != nil {
			return nil, err
		}
		db, err := c.App.projectDB(a.Str("project"))
		if err != nil {
			return nil, err
		}
		id := a.Str("bucket")
		if !validBucketID(id) {
			return nil, errBad("invalid_bucket", "bucket names use letters, numbers, dots, hyphens and underscores (max 63)", "")
		}
		var limit any
		if _, ok := a["file_size_limit"]; ok {
			if limit, err = parseSize(a.Str("file_size_limit")); err != nil {
				return nil, err
			}
		}
		var mimes any
		if l := a.StrList("allowed_mime_types"); len(l) > 0 {
			mimes = pq.Array(l)
		}
		if create {
			_, err = db.ExecContext(c, `INSERT INTO storage.buckets(id, name, public, file_size_limit, allowed_mime_types) VALUES ($1,$1,$2,$3,$4)`,
				id, a.Bool("public", false), limit, mimes)
			if err != nil && strings.Contains(err.Error(), "duplicate") {
				return nil, &APIError{Status: 409, Code: "bucket_exists", Message: "a bucket named " + id + " already exists"}
			}
		} else {
			sets, args := []string{}, []any{id}
			if _, ok := a["public"]; ok {
				args = append(args, a.Bool("public", false))
				sets = append(sets, fmt.Sprintf("public=$%d", len(args)))
			}
			if _, ok := a["file_size_limit"]; ok {
				args = append(args, limit)
				sets = append(sets, fmt.Sprintf("file_size_limit=$%d", len(args)))
			}
			if _, ok := a["allowed_mime_types"]; ok {
				args = append(args, mimes)
				sets = append(sets, fmt.Sprintf("allowed_mime_types=$%d", len(args)))
			}
			if len(sets) > 0 {
				var res sql.Result
				res, err = db.ExecContext(c, `UPDATE storage.buckets SET `+strings.Join(sets, ",")+`, updated_at=now() WHERE id=$1`, args...)
				if err == nil {
					if n, _ := res.RowsAffected(); n == 0 {
						return nil, errNotFound("bucket_not_found", "no bucket "+id, "")
					}
				}
			}
		}
		if err != nil {
			return nil, sqlError(err)
		}
		return map[string]any{"bucket": id, "saved": true}, nil
	}
	r.add(&Op{Name: "storage.bucket_create", Tag: "storage", Method: "POST", Path: "/api/v1/projects/{project}/storage/buckets", Level: LevelWrite,
		Summary: "Create a bucket", Params: append([]Param{projectParam, bucketParam}, bucketFields...),
		Run: func(c *Ctx, a Args) (any, error) { return setBucket(c, a, true) }})
	r.add(&Op{Name: "storage.bucket_update", Tag: "storage", Method: "PATCH", Path: "/api/v1/projects/{project}/storage/buckets/{bucket}", Level: LevelWrite,
		Summary: "Change a bucket's visibility, size limit or allowed types", Params: append([]Param{projectParam, bucketParam}, bucketFields...),
		Run: func(c *Ctx, a Args) (any, error) { return setBucket(c, a, false) }})

	bucketPreview := func(c *Ctx, a Args) (any, error) {
		db, err := c.App.projectDB(a.Str("project"))
		if err != nil {
			return nil, err
		}
		var n, size int64
		db.QueryRowContext(c, `SELECT count(*), coalesce(sum((metadata->>'size')::bigint),0) FROM storage.objects WHERE bucket_id=$1`, a.Str("bucket")).Scan(&n, &size)
		return map[string]any{"bucket": a.Str("bucket"), "files_deleted": n, "bytes_deleted": size}, nil
	}
	r.add(&Op{Name: "storage.bucket_empty", Tag: "storage", Method: "POST", Path: "/api/v1/projects/{project}/storage/buckets/{bucket}/empty", Level: LevelWrite,
		Destructive: true, Summary: "Delete every file in a bucket", Params: []Param{projectParam, bucketParam}, Preview: bucketPreview,
		Run: func(c *Ctx, a Args) (any, error) {
			removed, err := c.App.removeObjects(c, a.Str("project"), serviceClaims, c.App.storageBackend(), a.Str("bucket"), nil)
			if err != nil {
				return nil, err
			}
			return map[string]any{"deleted": len(removed)}, nil
		}})
	r.add(&Op{Name: "storage.bucket_delete", Tag: "storage", Method: "DELETE", Path: "/api/v1/projects/{project}/storage/buckets/{bucket}", Level: LevelWrite,
		Destructive: true, Summary: "Delete a bucket and every file in it", Params: []Param{projectParam, bucketParam}, Preview: bucketPreview,
		Run: func(c *Ctx, a Args) (any, error) {
			if _, err := c.App.removeObjects(c, a.Str("project"), serviceClaims, c.App.storageBackend(), a.Str("bucket"), nil); err != nil {
				return nil, err
			}
			db, err := c.App.projectDB(a.Str("project"))
			if err != nil {
				return nil, err
			}
			res, err := db.ExecContext(c, `DELETE FROM storage.buckets WHERE id=$1`, a.Str("bucket"))
			if err != nil {
				return nil, sqlError(err)
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return nil, errNotFound("bucket_not_found", "no bucket "+a.Str("bucket"), "")
			}
			return map[string]any{"deleted": a.Str("bucket")}, nil
		}})

	r.add(&Op{Name: "storage.objects_list", Tag: "storage", Method: "GET", Path: "/api/v1/projects/{project}/storage/buckets/{bucket}/objects", Level: LevelRead,
		Summary: "Files and folders directly under a prefix",
		Params: []Param{projectParam, bucketParam, {Name: "prefix", Type: "string"}, {Name: "search", Type: "string"},
			{Name: "limit", Type: "integer", Default: 200}, {Name: "offset", Type: "integer", Default: 0}},
		Run: func(c *Ctx, a Args) (any, error) {
			if err := ready(c, a); err != nil {
				return nil, err
			}
			return c.App.listObjects(c, a.Str("project"), serviceClaims, a.Str("bucket"), map[string]any{
				"prefix": a.Str("prefix"), "search": a.Str("search"), "limit": float64(a.Int("limit", 200)), "offset": float64(a.Int("offset", 0))})
		}})

	r.add(&Op{Name: "storage.object_delete", Tag: "storage", Method: "DELETE", Path: "/api/v1/projects/{project}/storage/buckets/{bucket}/objects", Level: LevelWrite,
		Destructive: true, Summary: "Delete files (full paths inside the bucket)",
		Params: []Param{projectParam, bucketParam, {Name: "paths", Type: "array", Required: true}},
		Preview: func(c *Ctx, a Args) (any, error) {
			return map[string]any{"bucket": a.Str("bucket"), "files": a.StrList("paths")}, nil
		},
		Run: func(c *Ctx, a Args) (any, error) {
			removed, err := c.App.removeObjects(c, a.Str("project"), serviceClaims, c.App.storageBackend(), a.Str("bucket"), a.StrList("paths"))
			if err != nil {
				return nil, err
			}
			return map[string]any{"deleted": len(removed)}, nil
		}})

	r.add(&Op{Name: "storage.object_move", Tag: "storage", Method: "POST", Path: "/api/v1/projects/{project}/storage/move", Level: LevelWrite,
		Summary: "Move / rename or copy a file",
		Params: []Param{projectParam, bucketParam, {Name: "from", Type: "string", Required: true}, {Name: "to", Type: "string", Required: true},
			{Name: "to_bucket", Type: "string"}, {Name: "copy", Type: "boolean", Default: false}},
		Run: func(c *Ctx, a Args) (any, error) {
			dst := a.Str("to_bucket")
			if dst == "" {
				dst = a.Str("bucket")
			}
			return c.App.moveObject(c, a.Str("project"), serviceClaims, c.App.storageBackend(), a.Str("bucket"), a.Str("from"), dst, a.Str("to"), a.Bool("copy", false))
		}})

	r.add(&Op{Name: "storage.signed_url", Tag: "storage", Method: "POST", Path: "/api/v1/projects/{project}/storage/sign", Level: LevelWrite,
		Summary: "A time-limited download link for a file (works for private buckets)",
		Params:  []Param{projectParam, bucketParam, {Name: "path", Type: "string", Required: true}, {Name: "expires_in", Type: "integer", Default: 3600}},
		Run: func(c *Ctx, a Args) (any, error) {
			if _, err := c.App.findObject(c, a.Str("project"), a.Str("bucket"), a.Str("path")); err != nil {
				return nil, err
			}
			st, err := c.App.projectAPIState(a.Str("project"))
			if err != nil {
				return nil, err
			}
			exp := clamp(a.Int("expires_in", 3600), 10, 60*60*24*365)
			url := c.App.projectURL(a.Str("project")) + "/storage/v1" + c.App.signedPath(a.Str("project"), st.Secret, a.Str("bucket"), a.Str("path"), exp)
			return map[string]any{"signed_url": url, "expires_in": exp,
				"public_url": c.App.projectURL(a.Str("project")) + "/storage/v1/object/public/" + a.Str("bucket") + "/" + urlPathEscape(a.Str("path"))}, nil
		}})

	r.add(&Op{Name: "storage.config_get", Tag: "storage", Method: "GET", Path: "/api/v1/storage/config", Level: LevelRead,
		Summary: "Where file bytes are stored (local disk or S3) and the upload size limit",
		Run: func(c *Ctx, a Args) (any, error) {
			cfg := c.App.storageConfig()
			if cfg.S3.SecretKey != "" {
				cfg.S3.SecretKey = masked
			}
			return map[string]any{"config": cfg, "local_dir": c.App.Cfg.Path("storage")}, nil
		}})

	r.add(&Op{Name: "storage.config_set", Tag: "storage", Method: "PUT", Path: "/api/v1/storage/config", Level: LevelAdmin,
		Summary:     "Store files on local disk or an S3-compatible bucket; set the upload size limit",
		Description: "Switching backends does not move existing files - set it before storing data, or copy the files across yourself.",
		Params: []Param{{Name: "backend", Type: "string", Enum: []string{"local", "s3"}}, {Name: "max_file_size_mb", Type: "integer"},
			{Name: "s3_endpoint", Type: "string"}, {Name: "s3_region", Type: "string"}, {Name: "s3_bucket", Type: "string"}, {Name: "s3_prefix", Type: "string"},
			{Name: "s3_access_key", Type: "string"}, {Name: "s3_secret_key", Type: "string", Secret: true}},
		Run: func(c *Ctx, a Args) (any, error) {
			cfg := StorageConfig{Backend: "local", MaxFileSizeMB: 50}
			c.App.getSetting("storage_config", &cfg)
			if _, ok := a["backend"]; ok {
				cfg.Backend = a.Str("backend")
			}
			if _, ok := a["max_file_size_mb"]; ok {
				cfg.MaxFileSizeMB = clamp(a.Int("max_file_size_mb", 50), 1, 50*1024)
			}
			set := func(k string, dst *string) {
				if v, ok := a[k]; ok && fmt.Sprint(v) != masked {
					*dst = strings.TrimSpace(a.Str(k))
				}
			}
			set("s3_endpoint", &cfg.S3.Endpoint)
			set("s3_region", &cfg.S3.Region)
			set("s3_bucket", &cfg.S3.Bucket)
			set("s3_prefix", &cfg.S3.Prefix)
			set("s3_access_key", &cfg.S3.AccessKey)
			set("s3_secret_key", &cfg.S3.SecretKey)
			if cfg.S3.Endpoint != "" && !strings.HasPrefix(cfg.S3.Endpoint, "http") {
				cfg.S3.Endpoint = "https://" + cfg.S3.Endpoint
			}
			if cfg.Backend == "s3" && !cfg.S3.configured() {
				return nil, errBad("s3_not_configured", "fill in endpoint, bucket, access key and secret key to use the s3 backend", "")
			}
			if err := c.App.setSetting("storage_config", cfg); err != nil {
				return nil, err
			}
			return c.App.Ops.Get("storage.config_get").Run(c, Args{})
		}})
}

// ---------------------------------------------------------------- UI upload / download (binary, not ops)

// handleStorageFile serves POST /api/v1/projects/{p}/storage/upload and
// GET /api/v1/projects/{p}/storage/download for the web UI.
func (s *Server) handleStorageFile(w http.ResponseWriter, r *http.Request, p *Principal, slug, action string) {
	if !p.canProject(slug) {
		writeError(w, errForbidden("not allowed for this project", ""))
		return
	}
	need := LevelRead
	if action == "upload" {
		need = LevelWrite
	}
	if p.Level < need {
		writeError(w, errForbidden("this needs "+need.String()+" access", ""))
		return
	}
	if err := s.app.ensureProjectAPIOnce(slug); err != nil {
		writeError(w, err)
		return
	}
	st, err := s.app.projectAPIState(slug)
	if err != nil {
		writeError(w, err)
		return
	}
	q := r.URL.Query()
	x := &storeReq{app: s.app, slug: slug, secret: st.Secret, claims: serviceClaims, w: w, r: r, be: s.app.storageBackend()}
	switch {
	case action == "upload" && r.Method == http.MethodPost:
		res, err := x.upload(r.Context(), q.Get("bucket"), q.Get("path"), q.Get("upsert") == "true", "")
		if err != nil {
			writeStorageError(w, err)
			return
		}
		if p.Kind != "system" {
			s.app.audit(p, &Op{Name: "storage.upload", Level: LevelWrite}, Args{"project": slug, "bucket": q.Get("bucket"), "path": q.Get("path")}, nil, 0)
		}
		writeJSON(w, 200, res)
	case action == "download" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		o, err := s.app.findObject(r.Context(), slug, q.Get("bucket"), q.Get("path"))
		if err != nil {
			writeStorageError(w, err)
			return
		}
		x.sendObject(o)
	default:
		writeError(w, &APIError{Status: 405, Code: "method_not_allowed", Message: "use POST .../storage/upload or GET .../storage/download"})
	}
}

// deleteProjectStorage removes a project's files from the backend.
func (a *App) deleteProjectStorage(ctx context.Context, slug string) {
	closeAPIDB(slug)
	if db, err := a.projectDB(slug); err == nil {
		rows, err := db.QueryContext(ctx, `SELECT bucket_id, id FROM storage.objects`)
		if err == nil {
			be := a.storageBackend()
			for rows.Next() {
				var b, id string
				rows.Scan(&b, &id)
				be.remove(ctx, slug, b, id)
			}
			rows.Close()
		}
	}
}
