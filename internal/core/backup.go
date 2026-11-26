package core

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/lib/pq"
)

// ---------------------------------------------------------------- config

type BackupConfig struct {
	Enabled       bool `json:"enabled"`
	ScheduleHours int  `json:"schedule_hours"` // run every N hours
	KeepLast      int  `json:"keep_last"`      // per project
	// KeepLocal is how many bundles per project also stay on this machine once
	// they are safely off-site. Ignored without S3 (then KeepLast applies locally).
	KeepLocal   int      `json:"keep_local"`
	VerifyAfter bool     `json:"verify_after"` // restore-test each scheduled backup
	S3          S3Config `json:"s3"`
}

func (a *App) backupConfig() BackupConfig {
	c := BackupConfig{Enabled: true, ScheduleHours: 24, KeepLast: 7, VerifyAfter: true}
	a.getSetting("backup_config", &c)
	if c.KeepLast < 1 {
		c.KeepLast = 1
	}
	if c.KeepLocal < 0 {
		c.KeepLocal = 0
	}
	return c
}

// pruneLocalCopies deletes local bundle files that already sit off-site,
// keeping only the newest cfg.KeepLocal per project. The S3 copy stays, and
// fetchBundle downloads it again for a restore or verify.
func (a *App) pruneLocalCopies(slug string, cfg BackupConfig) {
	if !cfg.S3.configured() {
		return
	}
	rows, err := a.Meta.Query(`SELECT id FROM backups WHERE project=$1 AND remote AND local ORDER BY created_at DESC OFFSET $2`, slug, cfg.KeepLocal)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if err := os.Remove(a.backupFile(id)); err != nil && !os.IsNotExist(err) {
			log.Printf("remove local backup %s: %v", id, err)
			continue
		}
		a.Meta.Exec(`UPDATE backups SET local=false WHERE id=$1`, id)
	}
}

type backupRunResult struct {
	At      time.Time `json:"at"`
	OK      bool      `json:"ok"`
	Created int       `json:"created"`
	Errors  []string  `json:"errors"`
}

// ---------------------------------------------------------------- bundle

type bundleManifest struct {
	Format     int               `json:"format"`
	Version    string            `json:"ivarabase_version"`
	ID         string            `json:"id"`
	CreatedAt  time.Time         `json:"created_at"`
	Kind       string            `json:"kind"`
	PGVersion  string            `json:"pg_version"`
	Project    bundleProject     `json:"project"`
	Tables     map[string]int64  `json:"tables"`
	RowsExact  bool              `json:"rows_exact"`
	Functions  []bundleFunction  `json:"functions"`
	Secrets    map[string]string `json:"secrets"`
	Migrations []bundleMigration `json:"migrations"`
	DumpBytes  int64             `json:"dump_bytes"`
	AuthConfig *AuthConfig       `json:"auth_config,omitempty"`
	// Storage files are included when they live on local disk. With the S3
	// backend they already sit in object storage and are not copied.
	StorageBackend  string `json:"storage_backend,omitempty"`
	StorageIncluded bool   `json:"storage_included"`
	StorageFiles    int    `json:"storage_files"`
}

type bundleProject struct {
	Slug  string `json:"slug"`
	Name  string `json:"name"`
	Notes string `json:"notes"`
}

type bundleFunction struct {
	Name       string            `json:"name"`
	Code       string            `json:"code"`
	Files      map[string]string `json:"files,omitempty"`
	Entrypoint string            `json:"entrypoint,omitempty"`
	Public     bool              `json:"public"`
	VerifyJWT  *bool             `json:"verify_jwt,omitempty"`
	Schedule   string            `json:"schedule,omitempty"`
	TimeoutS   int               `json:"timeout_s"`
	MemoryMB   int               `json:"memory_mb"`
}

type bundleMigration struct {
	Version   string    `json:"version"`
	Name      string    `json:"name"`
	SQL       string    `json:"sql"`
	Checksum  string    `json:"checksum"`
	AppliedAt time.Time `json:"applied_at"`
	AppliedBy string    `json:"applied_by"`
}

var backupIDRe = regexp.MustCompile(`^(\d{8}T\d{6}Z)-([a-z][a-z0-9_]{2,39})-([0-9a-f]{4})$`)

func newBackupID(slug string) string {
	return time.Now().UTC().Format("20060102T150405Z") + "-" + slug + "-" + randHex(2)
}

func (a *App) backupFile(id string) string { return a.Cfg.Path("backups", id+".ivbak") }

// tableCounts returns schema.table -> rows (exact for small databases).
func tableCounts(ctx context.Context, db *sql.DB, exact bool) (map[string]int64, error) {
	rows, err := db.QueryContext(ctx, `SELECT n.nspname, c.relname, greatest(c.reltuples,0)::bigint FROM pg_class c
		JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE c.relkind IN ('r','p') AND NOT c.relispartition AND n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_toast%'`)
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for rows.Next() {
		var s, t string
		var n int64
		rows.Scan(&s, &t, &n)
		out[s+"."+t] = n
	}
	rows.Close()
	if exact && len(out) <= 500 {
		for k := range out {
			parts := strings.SplitN(k, ".", 2)
			var n int64
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM `+qualified(parts[0], parts[1])).Scan(&n); err == nil {
				out[k] = n
			}
		}
	}
	return out, nil
}

// createBackup dumps one project into an encrypted bundle, uploads it off-site
// when configured, and applies retention.
func (a *App) createBackup(ctx context.Context, slug, kind string) (map[string]any, error) {
	a.backupMu.Lock()
	defer a.backupMu.Unlock()
	return a.createBackupLocked(ctx, slug, kind)
}

func (a *App) createBackupLocked(ctx context.Context, slug, kind string) (map[string]any, error) {
	p, err := a.project(slug)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	id := newBackupID(slug)
	work, err := os.MkdirTemp(a.Cfg.Path("tmp"), "bk-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)

	// 1. consistent row counts + dump (pg_dump takes a snapshot itself)
	sdb, err := a.PG.Super(slug)
	if err != nil {
		return nil, err
	}
	sizes := a.dbSizes()
	exact := sizes[slug].size < 2<<30
	counts, err := tableCounts(ctx, sdb, exact)
	a.PG.ClosePool(slug)
	if err != nil {
		return nil, err
	}
	dump := filepath.Join(work, "db.dump")
	if err := a.pgDump(ctx, slug, dump); err != nil {
		return nil, err
	}
	dst, _ := os.Stat(dump)

	// 2. manifest with everything needed to rebuild the project elsewhere
	m := bundleManifest{Format: 1, Version: Version, ID: id, CreatedAt: time.Now().UTC(), Kind: kind,
		PGVersion: a.PG.Version, Project: bundleProject{p.Slug, p.Name, p.Notes},
		Tables: counts, RowsExact: exact, Secrets: map[string]string{}, DumpBytes: dst.Size()}
	if rows, err := a.Meta.Query(`SELECT name,code,files,entrypoint,public,verify_jwt,schedule,timeout_s,memory_mb FROM functions WHERE project=$1`, slug); err == nil {
		for rows.Next() {
			var f bundleFunction
			var files []byte
			var vj bool
			rows.Scan(&f.Name, &f.Code, &files, &f.Entrypoint, &f.Public, &vj, &f.Schedule, &f.TimeoutS, &f.MemoryMB)
			json.Unmarshal(files, &f.Files)
			f.VerifyJWT = &vj
			m.Functions = append(m.Functions, f)
		}
		rows.Close()
	}
	if rows, err := a.Meta.Query(`SELECT name,value_enc FROM secrets WHERE project=$1`, slug); err == nil {
		for rows.Next() {
			var n, enc string
			rows.Scan(&n, &enc)
			if v, err := unseal(a.masterKey, enc); err == nil {
				m.Secrets[n] = v
			}
		}
		rows.Close()
	}
	if rows, err := a.Meta.Query(`SELECT version,name,sql,checksum,applied_at,applied_by FROM migrations WHERE project=$1 ORDER BY version`, slug); err == nil {
		for rows.Next() {
			var mg bundleMigration
			rows.Scan(&mg.Version, &mg.Name, &mg.SQL, &mg.Checksum, &mg.AppliedAt, &mg.AppliedBy)
			m.Migrations = append(m.Migrations, mg)
		}
		rows.Close()
	}

	if ac := a.authConfig(slug); true {
		m.AuthConfig = &ac
	}
	files := a.storageFilesForBackup(ctx, slug, &m)

	// 3. tar -> encrypt -> file, hashing the encrypted bytes
	final := a.backupFile(id)
	sum, size, err := writeBundle(final+".partial", a.recoveryKey, m, dump, files)
	if err != nil {
		os.Remove(final + ".partial")
		return nil, err
	}
	if err := os.Rename(final+".partial", final); err != nil {
		return nil, err
	}
	if _, err := a.Meta.Exec(`INSERT INTO backups(id,project,project_name,created_at,kind,size,sha256,pg_version,tables)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, id, slug, p.Name, m.CreatedAt, kind, size, sum, a.PG.Version, len(counts)); err != nil {
		return nil, err
	}

	// 4. off-site copy
	cfg := a.backupConfig()
	remoteErr := ""
	if cfg.S3.configured() {
		uctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		if err := cfg.S3.PutFile(uctx, id+".ivbak", final); err != nil {
			remoteErr = err.Error()
		} else {
			a.Meta.Exec(`UPDATE backups SET remote=true WHERE id=$1`, id)
		}
		cancel()
	}
	a.pruneBackups(ctx, slug, cfg)
	// scheduled runs verify right after this; drop the local copy after the
	// verify so it doesn't have to download the bundle again
	if !(kind == "scheduled" && cfg.VerifyAfter) {
		a.pruneLocalCopies(slug, cfg)
	}
	a.writeCatalog(ctx)

	out := map[string]any{"id": id, "project": slug, "kind": kind, "size_bytes": size, "sha256": sum,
		"tables": len(counts), "ms": time.Since(start).Milliseconds(), "offsite": cfg.S3.configured() && remoteErr == ""}
	if remoteErr != "" {
		out["offsite_error"] = remoteErr
	}
	return out, nil
}

func (a *App) pgDump(ctx context.Context, db, out string) error {
	try := func(compress string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, a.PG.bin("pg_dump"), "-Fc", "-Z", compress, "--no-password", "-f", out, "-d", db)
		cmd.Env = a.PG.toolEnv(db)
		return cmd.CombinedOutput()
	}
	if a.PG.Major >= 16 {
		if _, err := try("zstd:5"); err == nil {
			return nil
		}
	}
	if b, err := try("6"); err != nil {
		return fmt.Errorf("pg_dump failed: %v: %s", err, tail(string(b), 800))
	}
	return nil
}

type bundleFile struct{ name, path string }

// storageFilesForBackup lists the project's stored files (local backend only).
func (a *App) storageFilesForBackup(ctx context.Context, slug string, m *bundleManifest) []bundleFile {
	m.StorageBackend = a.storageConfig().Backend
	if m.StorageBackend != "local" {
		return nil
	}
	m.StorageIncluded = true
	db, err := a.projectDB(slug)
	if err != nil {
		return nil
	}
	rows, err := db.QueryContext(ctx, `SELECT bucket_id, id FROM storage.objects`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	lb := &localBackend{root: a.Cfg.Path("storage")}
	var out []bundleFile
	for rows.Next() {
		var b, id string
		rows.Scan(&b, &id)
		p := lb.path(slug, b, id)
		if fileExists(p) {
			out = append(out, bundleFile{name: "storage/" + safeSegment(b) + "/" + id, path: p})
		}
	}
	m.StorageFiles = len(out)
	return out
}

func writeBundle(path, key string, m bundleManifest, dump string, files []bundleFile) (string, int64, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	cw := &countWriter{w: io.MultiWriter(f, h)}
	enc, err := newEncWriter(cw, key)
	if err != nil {
		return "", 0, err
	}
	tw := tar.NewWriter(enc)
	mb, _ := json.MarshalIndent(m, "", "  ")
	if err := tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o600, Size: int64(len(mb)), ModTime: m.CreatedAt}); err != nil {
		return "", 0, err
	}
	tw.Write(mb)
	df, err := os.Open(dump)
	if err != nil {
		return "", 0, err
	}
	defer df.Close()
	st, _ := df.Stat()
	if err := tw.WriteHeader(&tar.Header{Name: "db.dump", Mode: 0o600, Size: st.Size(), ModTime: m.CreatedAt}); err != nil {
		return "", 0, err
	}
	if _, err := io.Copy(tw, df); err != nil {
		return "", 0, err
	}
	for _, bf := range files {
		f, err := os.Open(bf.path)
		if err != nil {
			continue // deleted meanwhile
		}
		st, _ := f.Stat()
		if err := tw.WriteHeader(&tar.Header{Name: bf.name, Mode: 0o600, Size: st.Size(), ModTime: st.ModTime()}); err != nil {
			f.Close()
			return "", 0, err
		}
		_, err = io.Copy(tw, f)
		f.Close()
		if err != nil {
			return "", 0, err
		}
	}
	if err := tw.Close(); err != nil {
		return "", 0, err
	}
	if err := enc.Close(); err != nil {
		return "", 0, err
	}
	if err := f.Sync(); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), cw.n, nil
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// openBundle decrypts a bundle into dir and returns its manifest.
func openBundle(path, key, dir string) (*bundleManifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec, err := newDecReader(f, key)
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(dec)
	var m *bundleManifest
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch hdr.Name {
		case "manifest.json":
			b, err := io.ReadAll(io.LimitReader(tr, 256<<20))
			if err != nil {
				return nil, err
			}
			m = &bundleManifest{}
			if err := json.Unmarshal(b, m); err != nil {
				return nil, fmt.Errorf("bad manifest: %w", err)
			}
		default:
			if !strings.HasPrefix(hdr.Name, "storage/") {
				continue
			}
			parts := strings.Split(strings.TrimPrefix(hdr.Name, "storage/"), "/")
			if len(parts) != 2 || parts[0] == "" || parts[0] == ".." || !uuidLike(parts[1]) {
				return nil, fmt.Errorf("unexpected entry in bundle: %s", hdr.Name)
			}
			p := filepath.Join(dir, "storage", parts[0], parts[1])
			os.MkdirAll(filepath.Dir(p), 0o700)
			out, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return nil, err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return nil, err
			}
			out.Close()
		case "db.dump":
			out, err := os.OpenFile(filepath.Join(dir, "db.dump"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return nil, err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return nil, err
			}
			out.Close()
		}
	}
	// a decReader that reached EOF has verified the final chunk; read any
	// tar padding to be sure the whole stream authenticated.
	if _, err := io.Copy(io.Discard, dec); err != nil {
		return nil, err
	}
	if m == nil || !fileExists(filepath.Join(dir, "db.dump")) {
		return nil, errors.New("bundle is missing manifest.json or db.dump")
	}
	return m, nil
}

// fetchBundle makes sure the bundle is on local disk (downloading it from S3 if needed).
func (a *App) fetchBundle(ctx context.Context, id string) (string, func(), error) {
	local := a.backupFile(id)
	if fileExists(local) {
		return local, func() {}, nil
	}
	cfg := a.backupConfig()
	if !cfg.S3.configured() {
		return "", nil, errNotFound("backup_file_missing", "the bundle file is not on this machine and no S3 target is configured", "")
	}
	tmp := a.Cfg.Path("tmp", id+".download")
	if err := cfg.S3.GetToFile(ctx, id+".ivbak", tmp); err != nil {
		os.Remove(tmp)
		return "", nil, fmt.Errorf("download from S3: %w", err)
	}
	return tmp, func() { os.Remove(tmp) }, nil
}

func (a *App) pgRestore(ctx context.Context, dump, db, role string, keepACL bool) (warnings string, err error) {
	args := []string{"--no-owner", "--no-password", "-d", db}
	if !keepACL {
		args = append(args, "--no-acl") // restore tests only count rows; grants are irrelevant there
	}
	if role != "" {
		args = append(args, "--role="+role)
		list, err := a.preRestoreExtensions(ctx, dump, db, role)
		if err != nil {
			return "", err
		}
		if list != "" {
			defer os.Remove(list)
			args = append(args, "-L", list)
		}
	}
	args = append(args, dump)
	cmd := exec.CommandContext(ctx, a.PG.bin("pg_restore"), args...)
	cmd.Env = a.PG.toolEnv(db)
	out, runErr := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if runErr != nil {
		// exit 1 with "errors ignored on restore" means partial; callers check counts
		if strings.Contains(text, "errors ignored on restore") {
			return tail(text, 2000), nil
		}
		return "", fmt.Errorf("pg_restore failed: %v: %s", runErr, tail(text, 1200))
	}
	return tail(text, 2000), nil
}

var dumpExtRe = regexp.MustCompile(`(?m)^CREATE EXTENSION IF NOT EXISTS ("?[A-Za-z0-9_-]+"?) WITH SCHEMA ("?[A-Za-z0-9_]+"?);`)

// preRestoreExtensions creates the dump's extensions as the superuser before
// pg_restore runs as the project role, which may not create untrusted ones
// such as vector or postgis. It returns a restore list (pg_restore -L) that
// skips those entries and their schemas, or "" when there is nothing to do.
func (a *App) preRestoreExtensions(ctx context.Context, dump, db, role string) (string, error) {
	ddl, err := exec.CommandContext(ctx, a.PG.bin("pg_restore"), "--section=pre-data", "-f", "-", dump).Output()
	if err != nil {
		return "", nil // let the real restore report the problem
	}
	exts := map[string]string{}
	for _, m := range dumpExtRe.FindAllStringSubmatch(string(ddl), -1) {
		name, schema := strings.Trim(m[1], `"`), strings.Trim(m[2], `"`)
		if _, ok := allowedExtensions[name]; ok {
			exts[name] = schema
		}
	}
	if len(exts) == 0 {
		return "", nil
	}
	sc, err := a.PG.Super(db)
	if err != nil {
		return "", err
	}
	schemas := map[string]bool{}
	for name, schema := range exts {
		if schema != "public" && !schemas[schema] {
			schemas[schema] = true
			if _, err := sc.ExecContext(ctx, fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %s AUTHORIZATION %s`, pq.QuoteIdentifier(schema), pq.QuoteIdentifier(role))); err != nil {
				return "", err
			}
		}
		if _, err := sc.ExecContext(ctx, fmt.Sprintf(`CREATE EXTENSION IF NOT EXISTS %s WITH SCHEMA %s CASCADE`, pq.QuoteIdentifier(name), pq.QuoteIdentifier(schema))); err != nil {
			return "", fmt.Errorf("restore needs the %s extension: %w (%s)", name, err, allowedExtensions[name].Install)
		}
	}
	toc, err := exec.CommandContext(ctx, a.PG.bin("pg_restore"), "-l", dump).Output()
	if err != nil {
		return "", nil
	}
	var out strings.Builder
	for _, line := range strings.Split(string(toc), "\n") {
		skip := false
		for name := range exts {
			if strings.Contains(line, " EXTENSION - "+name+" ") || strings.HasSuffix(line, " EXTENSION - "+name) ||
				strings.Contains(line, " COMMENT - EXTENSION "+name+" ") {
				skip = true
			}
		}
		for schema := range schemas {
			if strings.Contains(line, " SCHEMA - "+schema+" ") {
				skip = true
			}
		}
		if skip && !strings.HasPrefix(line, ";") {
			line = ";" + line
		}
		out.WriteString(line + "\n")
	}
	f, err := os.CreateTemp(a.Cfg.Path("tmp"), "restore-*.list")
	if err != nil {
		return "", err
	}
	f.WriteString(out.String())
	f.Close()
	return f.Name(), nil
}

// compareCounts checks a restored database against the manifest.
func compareCounts(want, got map[string]int64, exact bool) []string {
	var problems []string
	for t, n := range want {
		g, ok := got[t]
		if !ok {
			problems = append(problems, "missing table "+t)
			continue
		}
		if exact && g != n {
			problems = append(problems, fmt.Sprintf("%s has %d rows, expected %d", t, g, n))
		}
	}
	sort.Strings(problems)
	return problems
}

// verifyBackup restores a bundle into a scratch database and checks every table.
func (a *App) verifyBackup(ctx context.Context, id string) (map[string]any, error) {
	a.backupMu.Lock()
	defer a.backupMu.Unlock()
	return a.verifyBackupLocked(ctx, id)
}

func (a *App) verifyBackupLocked(ctx context.Context, id string) (map[string]any, error) {
	start := time.Now()
	path, cleanup, err := a.fetchBundle(ctx, id)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	result := func(ok bool, detail string, extra map[string]any) (map[string]any, error) {
		a.Meta.Exec(`UPDATE backups SET verified_at=now(), verify_ok=$2, verify_detail=$3 WHERE id=$1`, id, ok, detail)
		out := map[string]any{"id": id, "ok": ok, "detail": detail, "ms": time.Since(start).Milliseconds()}
		for k, v := range extra {
			out[k] = v
		}
		return out, nil
	}
	if want := a.backupSHA(id); want != "" {
		if got, err := fileSHA256(path); err != nil || got != want {
			return result(false, "sha256 mismatch: the file changed since it was written", nil)
		}
	}
	work, err := os.MkdirTemp(a.Cfg.Path("tmp"), "vf-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	m, err := openBundle(path, a.recoveryKey, work)
	if err != nil {
		return result(false, err.Error(), nil)
	}
	// Restore as a throwaway unprivileged role: code inside a backup (index
	// expressions, defaults) never runs with superuser rights.
	scratch := "ivb_verify_" + randHex(4)
	sc, err := a.PG.Super("postgres")
	if err != nil {
		return nil, err
	}
	if _, err := sc.Exec(`CREATE ROLE ` + scratch + ` NOLOGIN`); err != nil {
		return nil, err
	}
	defer sc.Exec(`DROP ROLE IF EXISTS ` + scratch)
	if _, err := sc.Exec(`CREATE DATABASE ` + scratch + ` OWNER ` + scratch + ` STRATEGY FILE_COPY`); err != nil {
		return nil, err
	}
	defer a.PG.dropDatabase(scratch)
	warnings, err := a.pgRestore(ctx, filepath.Join(work, "db.dump"), scratch, scratch, false)
	if err != nil {
		return result(false, err.Error(), nil)
	}
	db, err := a.PG.Super(scratch)
	if err != nil {
		return nil, err
	}
	got, err := tableCounts(ctx, db, m.RowsExact)
	a.PG.ClosePool(scratch)
	if err != nil {
		return result(false, err.Error(), nil)
	}
	var rows int64
	for _, n := range got {
		rows += n
	}
	extra := map[string]any{"tables": len(got), "rows": rows, "rows_checked_exactly": m.RowsExact,
		"functions": len(m.Functions), "secrets": len(m.Secrets), "migrations": len(m.Migrations)}
	if m.StorageIncluded {
		var nFiles int
		filepath.Walk(filepath.Join(work, "storage"), func(p string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				nFiles++
			}
			return nil
		})
		extra["storage_files"] = nFiles
		if nFiles != m.StorageFiles {
			extra["problems"] = []string{fmt.Sprintf("bundle has %d storage files, manifest says %d", nFiles, m.StorageFiles)}
			return result(false, "storage files are missing from the bundle", extra)
		}
	}
	if problems := compareCounts(m.Tables, got, m.RowsExact); len(problems) > 0 {
		extra["problems"] = problems
		return result(false, fmt.Sprintf("%d table(s) differ from the backup manifest", len(problems)), extra)
	}
	if warnings != "" {
		extra["restore_warnings"] = warnings
	}
	return result(true, fmt.Sprintf("restored %d tables / %d rows into a scratch database and matched the manifest", len(got), rows), extra)
}

func (a *App) backupSHA(id string) string {
	var s string
	a.Meta.QueryRow(`SELECT sha256 FROM backups WHERE id=$1`, id).Scan(&s)
	return s
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// restoreBackup restores a bundle as a new project, or replaces an existing
// project. Replace restores into a side database first and only swaps when the
// restore checks out, after taking a safety backup of the current state.
func (a *App) restoreBackup(ctx context.Context, id, mode, target string) (map[string]any, error) {
	a.backupMu.Lock()
	defer a.backupMu.Unlock()
	start := time.Now()
	path, cleanup, err := a.fetchBundle(ctx, id)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	work, err := os.MkdirTemp(a.Cfg.Path("tmp"), "rs-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	m, err := openBundle(path, a.recoveryKey, work)
	if err != nil {
		return nil, err
	}
	dump := filepath.Join(work, "db.dump")

	if mode == "new" {
		if target == "" {
			target = m.Project.Slug
			if _, err := a.project(target); err == nil {
				target = trimSlug(m.Project.Slug+"_restored", 40)
			}
		}
		if err := validSlug(target); err != nil {
			return nil, err
		}
		if _, err := a.project(target); err == nil {
			return nil, &APIError{Status: 409, Code: "project_exists", Message: fmt.Sprintf("project %q already exists", target),
				Hint: "pick another target_project, or use mode=replace"}
		}
		name := m.Project.Name
		if target != m.Project.Slug {
			name += " (restored " + m.CreatedAt.Format("2006-01-02 15:04") + ")"
		}
		if _, err := a.createProjectDB(target, name, false); err != nil {
			return nil, err
		}
		a.Meta.Exec(`UPDATE projects SET notes=$2 WHERE slug=$1`, target, m.Project.Notes)
		warnings, err := a.pgRestore(ctx, dump, target, target, true)
		if err != nil {
			p, _ := a.project(target)
			if p != nil {
				a.deleteProject(p)
			}
			return nil, err
		}
		problems := a.checkRestored(ctx, target, m)
		a.importBundleMeta(target, m)
		a.importBundleFiles(ctx, target, work, m, false)
		if err := a.ensureProjectAPI(target); err != nil {
			log.Printf("restore %s: API setup: %v", target, err)
		}
		a.refreshHooks(ctx, target, true)
		return map[string]any{"restored": true, "mode": "new", "project": target, "from_backup": id,
			"backup_created_at": m.CreatedAt, "problems": problems, "warnings": warnings, "ms": time.Since(start).Milliseconds()}, nil
	}

	// replace
	if target == "" {
		target = m.Project.Slug
	}
	p, err := a.project(target)
	if err != nil {
		return nil, err
	}
	safety, err := a.createBackupLocked(ctx, target, "pre-restore")
	if err != nil {
		return nil, fmt.Errorf("could not take the safety backup, nothing was changed: %w", err)
	}
	side := trimSlug(target, 30) + "_rs" + randHex(3)
	sc, err := a.PG.Super("postgres")
	if err != nil {
		return nil, err
	}
	if _, err := sc.Exec(fmt.Sprintf(`CREATE DATABASE %s OWNER %s STRATEGY FILE_COPY`, pq.QuoteIdentifier(side), pq.QuoteIdentifier(p.Role))); err != nil {
		return nil, err
	}
	warnings, err := a.pgRestore(ctx, dump, side, p.Role, true)
	if err != nil {
		a.PG.dropDatabase(side)
		return nil, fmt.Errorf("restore failed, the live project was not touched: %w", err)
	}
	if problems := a.checkRestored(ctx, side, m); len(problems) > 0 {
		a.PG.dropDatabase(side)
		return nil, &APIError{Status: 500, Code: "restore_check_failed", Message: "the restored copy did not match the backup; the live project was not touched",
			Details: problems}
	}
	// swap: live -> old, side -> live, drop old
	a.Fn.StopProject(target)
	a.Rest.Stop(target)
	a.Hooks.stop(target)
	a.closeProjectDB(target)
	a.PG.ClosePool(side)
	old := trimSlug(target, 30) + "_old" + randHex(3)
	a.PG.terminate(target)
	a.PG.terminate(side)
	if _, err := sc.Exec(fmt.Sprintf(`ALTER DATABASE %s RENAME TO %s`, pq.QuoteIdentifier(target), pq.QuoteIdentifier(old))); err != nil {
		a.PG.dropDatabase(side)
		return nil, fmt.Errorf("could not swap databases (live project untouched): %w", err)
	}
	if _, err := sc.Exec(fmt.Sprintf(`ALTER DATABASE %s RENAME TO %s`, pq.QuoteIdentifier(side), pq.QuoteIdentifier(target))); err != nil {
		sc.Exec(fmt.Sprintf(`ALTER DATABASE %s RENAME TO %s`, pq.QuoteIdentifier(old), pq.QuoteIdentifier(target)))
		a.PG.dropDatabase(side)
		return nil, fmt.Errorf("could not swap databases (rolled back): %w", err)
	}
	sc.Exec(fmt.Sprintf(`REVOKE ALL ON DATABASE %s FROM PUBLIC`, pq.QuoteIdentifier(target)))
	a.PG.dropDatabase(old)
	for _, t := range []string{"functions", "secrets", "migrations"} {
		a.Meta.Exec(`DELETE FROM `+t+` WHERE project=$1`, target)
	}
	a.importBundleMeta(target, m)
	a.importBundleFiles(ctx, target, work, m, true)
	if err := a.ensureProjectAPI(target); err != nil {
		log.Printf("restore %s: API setup: %v", target, err)
	}
	a.refreshHooks(ctx, target, true)
	return map[string]any{"restored": true, "mode": "replace", "project": target, "from_backup": id,
		"backup_created_at": m.CreatedAt, "safety_backup": safety["id"], "warnings": warnings, "ms": time.Since(start).Milliseconds()}, nil
}

func (a *App) checkRestored(ctx context.Context, db string, m *bundleManifest) []string {
	sdb, err := a.PG.Super(db)
	if err != nil {
		return []string{err.Error()}
	}
	got, err := tableCounts(ctx, sdb, m.RowsExact)
	a.PG.ClosePool(db)
	if err != nil {
		return []string{err.Error()}
	}
	return compareCounts(m.Tables, got, m.RowsExact)
}

func (a *App) importBundleMeta(slug string, m *bundleManifest) {
	if m.AuthConfig != nil {
		a.saveAuthConfig(slug, *m.AuthConfig)
	}
	for _, f := range m.Functions {
		if f.Entrypoint == "" {
			f.Entrypoint = "index.ts"
		}
		if len(f.Files) == 0 {
			f.Files = map[string]string{f.Entrypoint: f.Code}
		}
		vj := !f.Public
		if f.VerifyJWT != nil {
			vj = *f.VerifyJWT
		}
		fj, _ := json.Marshal(f.Files)
		a.Meta.Exec(`INSERT INTO functions(project,name,code,files,entrypoint,public,verify_jwt,schedule,timeout_s,memory_mb) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
			ON CONFLICT (project,name) DO UPDATE SET code=$3, files=$4, entrypoint=$5, public=$6, verify_jwt=$7, schedule=$8, timeout_s=$9, memory_mb=$10,
			version=functions.version+1, updated_at=now()`,
			slug, f.Name, f.Code, fj, f.Entrypoint, !vj, vj, f.Schedule, f.TimeoutS, f.MemoryMB)
		if r, err := a.function(slug, f.Name); err == nil {
			a.writeFnFiles(r)
		}
	}
	for n, v := range m.Secrets {
		a.Meta.Exec(`INSERT INTO secrets(project,name,value_enc) VALUES ($1,$2,$3) ON CONFLICT (project,name) DO UPDATE SET value_enc=$3`,
			slug, n, seal(a.masterKey, v))
	}
	for _, mg := range m.Migrations {
		a.Meta.Exec(`INSERT INTO migrations(project,version,name,sql,checksum,applied_at,applied_by) VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT DO NOTHING`, slug, mg.Version, mg.Name, mg.SQL, mg.Checksum, mg.AppliedAt, mg.AppliedBy)
	}
}

func trimSlug(s string, n int) string {
	if len(s) > n {
		return strings.TrimRight(s[:n], "_")
	}
	return s
}

// ---------------------------------------------------------------- retention, catalog

func (a *App) pruneBackups(ctx context.Context, slug string, cfg BackupConfig) {
	rows, err := a.Meta.Query(`SELECT id FROM backups WHERE project=$1 ORDER BY created_at DESC OFFSET $2`, slug, cfg.KeepLast)
	if err != nil {
		return
	}
	var old []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		old = append(old, id)
	}
	rows.Close()
	for _, id := range old {
		a.removeBackup(ctx, id, cfg)
	}
}

func (a *App) removeBackup(ctx context.Context, id string, cfg BackupConfig) error {
	os.Remove(a.backupFile(id))
	if cfg.S3.configured() {
		if err := cfg.S3.Delete(ctx, id+".ivbak"); err != nil {
			log.Printf("delete %s from S3: %v", id, err)
		}
	}
	_, err := a.Meta.Exec(`DELETE FROM backups WHERE id=$1`, id)
	return err
}

type catalogEntry struct {
	ID        string    `json:"id"`
	Project   string    `json:"project"`
	CreatedAt time.Time `json:"created_at"`
	Kind      string    `json:"kind"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
}

// writeCatalog stores a plain-JSON index next to the bundles (locally and
// off-site) so a fresh machine can find the newest bundle per project.
func (a *App) writeCatalog(ctx context.Context) {
	rows, err := a.Meta.Query(`SELECT id,project,created_at,kind,size,sha256 FROM backups ORDER BY created_at`)
	if err != nil {
		return
	}
	cat := struct {
		Format    int            `json:"format"`
		UpdatedAt time.Time      `json:"updated_at"`
		Backups   []catalogEntry `json:"backups"`
	}{Format: 1, UpdatedAt: time.Now().UTC(), Backups: []catalogEntry{}}
	for rows.Next() {
		var e catalogEntry
		rows.Scan(&e.ID, &e.Project, &e.CreatedAt, &e.Kind, &e.Size, &e.SHA256)
		cat.Backups = append(cat.Backups, e)
	}
	rows.Close()
	b, _ := json.MarshalIndent(cat, "", "  ")
	writeFileAtomic(a.Cfg.Path("backups", "catalog.json"), b, 0o600)
	if cfg := a.backupConfig(); cfg.S3.configured() {
		if err := cfg.S3.PutBytes(ctx, "catalog.json", b); err != nil {
			log.Printf("upload catalog: %v", err)
		}
	}
}

// syncCatalog registers bundles found on disk or in S3 that the meta store
// does not know about (after a disaster recovery or a manual copy).
func (a *App) syncCatalog(ctx context.Context) (map[string]any, error) {
	found := map[string]catalogEntry{}
	local := map[string]bool{}
	remote := map[string]bool{}
	if entries, err := os.ReadDir(a.Cfg.Path("backups")); err == nil {
		for _, e := range entries {
			id := strings.TrimSuffix(e.Name(), ".ivbak")
			if !strings.HasSuffix(e.Name(), ".ivbak") || !backupIDRe.MatchString(id) {
				continue
			}
			info, _ := e.Info()
			found[id] = catalogEntry{ID: id, Size: info.Size()}
			local[id] = true
		}
	}
	cfg := a.backupConfig()
	var remoteErr string
	if cfg.S3.configured() {
		objs, err := cfg.S3.List(ctx)
		if err != nil {
			remoteErr = err.Error()
		}
		for _, o := range objs {
			id := strings.TrimSuffix(o.Key, ".ivbak")
			if !strings.HasSuffix(o.Key, ".ivbak") || !backupIDRe.MatchString(id) {
				continue
			}
			if _, ok := found[id]; !ok {
				found[id] = catalogEntry{ID: id, Size: o.Size}
			}
			remote[id] = true
		}
	}
	added := 0
	for id, e := range found {
		mm := backupIDRe.FindStringSubmatch(id)
		t, _ := time.Parse("20060102T150405Z", mm[1])
		res, err := a.Meta.Exec(`INSERT INTO backups(id,project,created_at,kind,size,sha256,local,remote) VALUES ($1,$2,$3,'imported',$4,'',$5,$6)
			ON CONFLICT (id) DO UPDATE SET local=$5, remote=$6`, id, mm[2], t, e.Size, local[id], remote[id])
		if err == nil {
			if n, _ := res.RowsAffected(); n > 0 {
				added++
			}
		}
	}
	a.writeCatalog(ctx)
	out := map[string]any{"bundles_found": len(found), "local": len(local), "remote": len(remote)}
	if remoteErr != "" {
		out["remote_error"] = remoteErr
	}
	return out, nil
}

// ---------------------------------------------------------------- scheduler

func (a *App) backupScheduler(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cfg := a.backupConfig()
		if !cfg.Enabled || cfg.ScheduleHours <= 0 {
			continue
		}
		var last backupRunResult
		a.getSetting("backup_last_run", &last)
		if time.Since(last.At) < time.Duration(cfg.ScheduleHours)*time.Hour {
			continue
		}
		a.runScheduledBackups(ctx, cfg)
	}
}

func (a *App) runScheduledBackups(ctx context.Context, cfg BackupConfig) backupRunResult {
	res := backupRunResult{At: time.Now().UTC(), OK: true, Errors: []string{}}
	rows, err := a.Meta.Query(`SELECT slug FROM projects ORDER BY slug`)
	if err != nil {
		res.OK = false
		res.Errors = append(res.Errors, err.Error())
		a.setSetting("backup_last_run", res)
		return res
	}
	var slugs []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		slugs = append(slugs, s)
	}
	rows.Close()
	for _, s := range slugs {
		b, err := a.createBackup(ctx, s, "scheduled")
		if err != nil {
			res.OK = false
			res.Errors = append(res.Errors, s+": "+err.Error())
			continue
		}
		res.Created++
		if e, ok := b["offsite_error"]; ok {
			res.OK = false
			res.Errors = append(res.Errors, fmt.Sprintf("%s: off-site upload failed: %v", s, e))
		}
		if cfg.VerifyAfter {
			v, err := a.verifyBackup(ctx, b["id"].(string))
			if err != nil {
				res.OK = false
				res.Errors = append(res.Errors, s+": verify: "+err.Error())
			} else if ok, _ := v["ok"].(bool); !ok {
				res.OK = false
				res.Errors = append(res.Errors, fmt.Sprintf("%s: verify failed: %v", s, v["detail"]))
			}
			a.pruneLocalCopies(s, cfg)
		}
	}
	a.setSetting("backup_last_run", res)
	if !res.OK {
		log.Printf("scheduled backups finished with errors: %v", res.Errors)
	}
	return res
}

// ---------------------------------------------------------------- disaster recovery

// RestoreAll rebuilds every project from the newest bundle per project in the
// local backup directory and/or the configured S3 target. Existing projects are
// skipped. Used by `ivarabase dr restore` on a fresh machine.
func (a *App) RestoreAll(ctx context.Context, only []string) ([]map[string]any, error) {
	if _, err := a.syncCatalog(ctx); err != nil {
		return nil, err
	}
	rows, err := a.Meta.Query(`SELECT DISTINCT ON (project) id, project FROM backups ORDER BY project, created_at DESC`)
	if err != nil {
		return nil, err
	}
	type pick struct{ id, project string }
	var picks []pick
	for rows.Next() {
		var p pick
		rows.Scan(&p.id, &p.project)
		picks = append(picks, p)
	}
	rows.Close()
	want := map[string]bool{}
	for _, o := range only {
		want[o] = true
	}
	var out []map[string]any
	for _, p := range picks {
		if len(want) > 0 && !want[p.project] {
			continue
		}
		if _, err := a.project(p.project); err == nil {
			out = append(out, map[string]any{"project": p.project, "skipped": "already exists"})
			continue
		}
		res, err := a.restoreBackup(ctx, p.id, "new", p.project)
		if err != nil {
			out = append(out, map[string]any{"project": p.project, "backup": p.id, "error": AsAPIError(err)})
			continue
		}
		out = append(out, res)
	}
	return out, nil
}

// CheckRecoveryKey decrypts the newest known bundle with the configured key,
// so disaster recovery fails fast on a mistyped key.
func (a *App) CheckRecoveryKey(ctx context.Context) error {
	if _, err := a.syncCatalog(ctx); err != nil {
		return err
	}
	var id string
	if err := a.Meta.QueryRow(`SELECT id FROM backups ORDER BY created_at DESC LIMIT 1`).Scan(&id); err != nil {
		return errNotFound("no_backups", "no backup bundles were found at the given source", "check --from or the --s3-* flags")
	}
	path, cleanup, err := a.fetchBundle(ctx, id)
	if err != nil {
		return err
	}
	defer cleanup()
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	dec, err := newDecReader(f, a.recoveryKey)
	if err != nil {
		return err
	}
	_, err = dec.Read(make([]byte, 512))
	return err
}

// ---------------------------------------------------------------- ops

func registerBackupOps(r *Registry) {
	idParam := Param{Name: "id", Type: "string", Required: true, Desc: "backup id"}

	r.add(&Op{Name: "backups.list", Tag: "backups", Method: "GET", Path: "/api/v1/backups", Level: LevelRead,
		Summary: "List backups, newest first, with verification status and where copies live",
		Params:  []Param{{Name: "project", Type: "string", Desc: "only this project"}, {Name: "limit", Type: "integer", Default: 200}},
		Run: func(c *Ctx, a Args) (any, error) {
			q := `SELECT id,project,project_name,created_at,kind,size,sha256,pg_version,tables,local,remote,verified_at,verify_ok,verify_detail FROM backups`
			var args []any
			if p := a.Str("project"); p != "" {
				q += ` WHERE project=$1`
				args = append(args, p)
			}
			q += fmt.Sprintf(` ORDER BY created_at DESC LIMIT %d`, clamp(a.Int("limit", 200), 1, 2000))
			rows, err := c.App.Meta.Query(q, args...)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			out := []map[string]any{}
			for rows.Next() {
				var (
					id, project, pname, kind, sum, pgv, vdetail string
					created                                     time.Time
					size                                        int64
					tables                                      int
					local, remote                               bool
					vat                                         sql.NullTime
					vok                                         sql.NullBool
				)
				if err := rows.Scan(&id, &project, &pname, &created, &kind, &size, &sum, &pgv, &tables, &local, &remote, &vat, &vok, &vdetail); err != nil {
					return nil, err
				}
				if !c.Principal.canProject(project) {
					continue
				}
				var verified any
				if vok.Valid {
					verified = vok.Bool
				}
				out = append(out, map[string]any{"id": id, "project": project, "project_name": pname, "created_at": created,
					"kind": kind, "size_bytes": size, "sha256": sum, "pg_version": pgv, "tables": tables,
					"local": local && fileExists(c.App.backupFile(id)), "offsite": remote,
					"verified_at": nt(vat), "verified": verified, "verify_detail": vdetail})
			}
			return out, rows.Err()
		}})

	r.add(&Op{Name: "backups.create", Tag: "backups", Method: "POST", Path: "/api/v1/backups", Level: LevelWrite,
		Summary:     "Back up one project (or all) now: encrypted bundle, off-site copy, retention",
		Description: "Take one before risky migrations. Omit project to back up every project the caller can access.",
		Params:      []Param{{Name: "project", Type: "string", Desc: "slug; omit for all projects"}, {Name: "verify", Type: "boolean", Default: false, Desc: "also restore-test it right away"}},
		Run: func(c *Ctx, a Args) (any, error) {
			var slugs []string
			if p := a.Str("project"); p != "" {
				slugs = []string{p}
			} else {
				rows, err := c.App.Meta.Query(`SELECT slug FROM projects ORDER BY slug`)
				if err != nil {
					return nil, err
				}
				for rows.Next() {
					var s string
					rows.Scan(&s)
					if c.Principal.canProject(s) {
						slugs = append(slugs, s)
					}
				}
				rows.Close()
			}
			var results []map[string]any
			for _, s := range slugs {
				b, err := c.App.createBackup(c, s, "manual")
				if err != nil {
					if len(slugs) == 1 {
						return nil, err
					}
					results = append(results, map[string]any{"project": s, "error": AsAPIError(err)})
					continue
				}
				if a.Bool("verify", false) {
					v, err := c.App.verifyBackup(c, b["id"].(string))
					if err != nil {
						b["verify"] = AsAPIError(err)
					} else {
						b["verify"] = v
					}
				}
				results = append(results, b)
			}
			if a.Str("project") != "" {
				return results[0], nil
			}
			return map[string]any{"backups": results}, nil
		}})

	r.add(&Op{Name: "backups.get", Tag: "backups", Method: "GET", Path: "/api/v1/backups/{id}", Level: LevelRead,
		Summary: "Details of one backup, including its manifest (tables, row counts, functions)",
		Params:  []Param{idParam, {Name: "include_manifest", Type: "boolean", Default: false, Desc: "decrypts the bundle header to show contents"}},
		Run: func(c *Ctx, a Args) (any, error) {
			id := a.Str("id")
			var project string
			if err := c.App.Meta.QueryRow(`SELECT project FROM backups WHERE id=$1`, id).Scan(&project); err != nil {
				return nil, errNotFound("backup_not_found", "no backup "+id, "call backups.list")
			}
			if !c.Principal.canProject(project) {
				return nil, errForbidden("not allowed for this project", "")
			}
			list, err := c.App.Ops.Get("backups.list").Run(c, Args{"project": project, "limit": 2000})
			if err != nil {
				return nil, err
			}
			var out map[string]any
			for _, b := range list.([]map[string]any) {
				if b["id"] == id {
					out = b
				}
			}
			if a.Bool("include_manifest", false) {
				path, cleanup, err := c.App.fetchBundle(c, id)
				if err != nil {
					return nil, err
				}
				defer cleanup()
				work, _ := os.MkdirTemp(c.App.Cfg.Path("tmp"), "mf-")
				defer os.RemoveAll(work)
				m, err := openBundle(path, c.App.recoveryKey, work)
				if err != nil {
					return nil, err
				}
				fns := []string{}
				for _, f := range m.Functions {
					fns = append(fns, f.Name)
				}
				secrets := []string{}
				for n := range m.Secrets {
					secrets = append(secrets, n)
				}
				sort.Strings(secrets)
				out["manifest"] = map[string]any{"project": m.Project, "created_at": m.CreatedAt, "pg_version": m.PGVersion,
					"tables": m.Tables, "rows_exact": m.RowsExact, "functions": fns, "secrets": secrets,
					"migrations": len(m.Migrations), "dump_bytes": m.DumpBytes}
			}
			return out, nil
		}})

	r.add(&Op{Name: "backups.verify", Tag: "backups", Method: "POST", Path: "/api/v1/backups/{id}/verify", Level: LevelWrite,
		Summary: "Prove a backup restores: decrypt, restore into a scratch database, compare every table, drop it",
		Params:  []Param{idParam},
		Run:     func(c *Ctx, a Args) (any, error) { return c.App.verifyBackup(c, a.Str("id")) }})

	r.add(&Op{Name: "backups.restore", Tag: "backups", Method: "POST", Path: "/api/v1/backups/{id}/restore", Level: LevelWrite,
		Summary:       "Restore a backup as a new project (safe) or over an existing one (replace)",
		Description:   "mode=new creates a separate project and never touches existing data. mode=replace first takes a safety backup, restores into a side database, checks it, and only then swaps it in - so a failed restore leaves the live project untouched.",
		DestructiveIf: func(a Args) bool { return a.Str("mode") == "replace" },
		Params: []Param{idParam,
			{Name: "mode", Type: "string", Enum: []string{"new", "replace"}, Default: "new"},
			{Name: "target_project", Type: "string", Desc: "new: slug for the new project (default <slug>_restored); replace: project to overwrite (default the original)"}},
		Preview: func(c *Ctx, a Args) (any, error) {
			var project string
			var created time.Time
			if err := c.App.Meta.QueryRow(`SELECT project, created_at FROM backups WHERE id=$1`, a.Str("id")).Scan(&project, &created); err != nil {
				return nil, errNotFound("backup_not_found", "no backup "+a.Str("id"), "")
			}
			target := a.Str("target_project")
			if target == "" {
				target = project
			}
			return map[string]any{"overwrites_project": target, "with_state_from": created,
				"data_lost":  "every change in " + target + " since " + created.Format(time.RFC3339),
				"safety_net": "a pre-restore backup of the current state is taken first"}, nil
		},
		Run: func(c *Ctx, a Args) (any, error) {
			var project string
			if err := c.App.Meta.QueryRow(`SELECT project FROM backups WHERE id=$1`, a.Str("id")).Scan(&project); err != nil {
				return nil, errNotFound("backup_not_found", "no backup "+a.Str("id"), "call backups.list or backups.sync")
			}
			target := a.Str("target_project")
			if !c.Principal.canProject(project) || (target != "" && !c.Principal.canProject(target)) {
				return nil, errForbidden("not allowed for this project", "")
			}
			if a.Str("mode") == "new" && c.Principal.Kind == "token" && len(c.Principal.Projects) > 0 {
				return nil, errForbidden("project-scoped tokens cannot create projects; use mode=replace or an unscoped token", "")
			}
			return c.App.restoreBackup(c, a.Str("id"), a.Str("mode"), target)
		}})

	r.add(&Op{Name: "backups.delete", Tag: "backups", Method: "DELETE", Path: "/api/v1/backups/{id}", Level: LevelWrite,
		Destructive: true,
		Summary:     "Delete a backup locally and off-site",
		Params:      []Param{idParam},
		Preview: func(c *Ctx, a Args) (any, error) {
			var project string
			var created time.Time
			if err := c.App.Meta.QueryRow(`SELECT project, created_at FROM backups WHERE id=$1`, a.Str("id")).Scan(&project, &created); err != nil {
				return nil, errNotFound("backup_not_found", "no backup "+a.Str("id"), "")
			}
			var remaining int
			c.App.Meta.QueryRow(`SELECT count(*) FROM backups WHERE project=$1 AND id<>$2`, project, a.Str("id")).Scan(&remaining)
			return map[string]any{"backup": a.Str("id"), "project": project, "created_at": created, "other_backups_of_project": remaining}, nil
		},
		Run: func(c *Ctx, a Args) (any, error) {
			var project string
			if err := c.App.Meta.QueryRow(`SELECT project FROM backups WHERE id=$1`, a.Str("id")).Scan(&project); err != nil {
				return nil, errNotFound("backup_not_found", "no backup "+a.Str("id"), "")
			}
			if !c.Principal.canProject(project) {
				return nil, errForbidden("not allowed for this project", "")
			}
			if err := c.App.removeBackup(c, a.Str("id"), c.App.backupConfig()); err != nil {
				return nil, err
			}
			c.App.writeCatalog(c)
			return map[string]any{"deleted": a.Str("id")}, nil
		}})

	r.add(&Op{Name: "backups.sync", Tag: "backups", Method: "POST", Path: "/api/v1/backups/sync", Level: LevelAdmin,
		Summary: "Register bundles found in the backup folder or S3 bucket (after recovery or a manual copy)",
		Run:     func(c *Ctx, a Args) (any, error) { return c.App.syncCatalog(c) }})

	r.add(&Op{Name: "backups.run_schedule", Tag: "backups", Method: "POST", Path: "/api/v1/backups/run-schedule", Level: LevelAdmin,
		Summary: "Run the scheduled job now: back up every project, verify, prune",
		Run:     func(c *Ctx, a Args) (any, error) { return c.App.runScheduledBackups(c, c.App.backupConfig()), nil }})

	r.add(&Op{Name: "backups.config_get", Tag: "backups", Method: "GET", Path: "/api/v1/backups/config", Level: LevelRead,
		Summary: "Backup schedule, retention, verification and off-site target (secret key hidden)",
		Run: func(c *Ctx, a Args) (any, error) {
			cfg := c.App.backupConfig()
			if cfg.S3.SecretKey != "" {
				cfg.S3.SecretKey = "********"
			}
			var last backupRunResult
			out := map[string]any{"config": cfg, "local_dir": c.App.Cfg.Path("backups"),
				"recovery_key_file": c.App.Cfg.Path("secrets", "recovery.key"),
				"encryption":        "AES-256-GCM, key derived from the recovery key (PBKDF2-SHA256)"}
			if c.App.getSetting("backup_last_run", &last) {
				out["last_run"] = last
				out["next_run_at"] = last.At.Add(time.Duration(cfg.ScheduleHours) * time.Hour)
			}
			return out, nil
		}})

	r.add(&Op{Name: "backups.config_set", Tag: "backups", Method: "PUT", Path: "/api/v1/backups/config", Level: LevelAdmin,
		Summary: "Change schedule, retention, verification or the S3-compatible off-site target",
		Params: []Param{
			{Name: "enabled", Type: "boolean"},
			{Name: "schedule_hours", Type: "integer", Desc: "run every N hours (1-168)"},
			{Name: "keep_last", Type: "integer", Desc: "backups kept per project (1-365)"},
			{Name: "keep_local", Type: "integer", Desc: "with an S3 target: how many of those also stay on this machine once uploaded (0-365, default 0)"},
			{Name: "verify_after", Type: "boolean", Desc: "restore-test every scheduled backup"},
			{Name: "s3_endpoint", Type: "string", Desc: "e.g. https://s3.eu-central-1.amazonaws.com or https://<account>.r2.cloudflarestorage.com; empty string disables off-site"},
			{Name: "s3_region", Type: "string", Desc: "auto for R2, us-east-1 default"},
			{Name: "s3_bucket", Type: "string"},
			{Name: "s3_prefix", Type: "string", Desc: "folder inside the bucket, e.g. ivarabase/"},
			{Name: "s3_access_key", Type: "string"},
			{Name: "s3_secret_key", Type: "string", Secret: true}},
		Run: func(c *Ctx, a Args) (any, error) {
			cfg := c.App.backupConfig()
			if _, ok := a["enabled"]; ok {
				cfg.Enabled = a.Bool("enabled", cfg.Enabled)
			}
			if _, ok := a["schedule_hours"]; ok {
				cfg.ScheduleHours = clamp(a.Int("schedule_hours", 24), 1, 168)
			}
			if _, ok := a["keep_last"]; ok {
				cfg.KeepLast = clamp(a.Int("keep_last", 7), 1, 365)
			}
			if _, ok := a["keep_local"]; ok {
				cfg.KeepLocal = clamp(a.Int("keep_local", 0), 0, 365)
			}
			if _, ok := a["verify_after"]; ok {
				cfg.VerifyAfter = a.Bool("verify_after", true)
			}
			set := func(k string, dst *string) {
				if _, ok := a[k]; ok {
					*dst = strings.TrimSpace(a.Str(k))
				}
			}
			set("s3_endpoint", &cfg.S3.Endpoint)
			set("s3_region", &cfg.S3.Region)
			set("s3_bucket", &cfg.S3.Bucket)
			set("s3_prefix", &cfg.S3.Prefix)
			set("s3_access_key", &cfg.S3.AccessKey)
			if v := a.Str("s3_secret_key"); v != "" && v != "********" {
				cfg.S3.SecretKey = v
			}
			if cfg.S3.Endpoint != "" && !strings.HasPrefix(cfg.S3.Endpoint, "http") {
				cfg.S3.Endpoint = "https://" + cfg.S3.Endpoint
			}
			if err := c.App.setSetting("backup_config", cfg); err != nil {
				return nil, err
			}
			cfg.S3.SecretKey = strings.Repeat("*", min(8, len(cfg.S3.SecretKey)))
			return map[string]any{"config": cfg, "saved": true}, nil
		}})

	r.add(&Op{Name: "backups.test_target", Tag: "backups", Method: "POST", Path: "/api/v1/backups/config/test", Level: LevelAdmin,
		Summary: "Check the S3 target: write, list, read back and delete a small test object",
		Run: func(c *Ctx, a Args) (any, error) {
			cfg := c.App.backupConfig()
			if !cfg.S3.configured() {
				return nil, errBad("s3_not_configured", "set s3_endpoint, s3_bucket, s3_access_key and s3_secret_key first", "")
			}
			ctx, cancel := context.WithTimeout(c, 30*time.Second)
			defer cancel()
			key := "ivarabase-test-" + randHex(4) + ".txt"
			steps := []map[string]any{}
			step := func(name string, err error) bool {
				s := map[string]any{"step": name, "ok": err == nil}
				if err != nil {
					s["error"] = err.Error()
				}
				steps = append(steps, s)
				return err == nil
			}
			ok := step("put", cfg.S3.PutBytes(ctx, key, []byte("ok")))
			if ok {
				_, err := cfg.S3.List(ctx)
				ok = step("list", err)
			}
			if ok {
				tmp := c.App.Cfg.Path("tmp", key)
				ok = step("get", cfg.S3.GetToFile(ctx, key, tmp))
				os.Remove(tmp)
			}
			step("delete", cfg.S3.Delete(ctx, key))
			return map[string]any{"ok": ok, "steps": steps}, nil
		}})
}

// importBundleFiles puts a bundle's storage files into the local backend.
// replace=true first clears the project's current files (the safety backup
// taken before a replace-restore still has them).
func (a *App) importBundleFiles(ctx context.Context, slug, work string, m *bundleManifest, replace bool) {
	closeAPIDB(slug)
	if !m.StorageIncluded || a.storageConfig().Backend != "local" {
		return
	}
	root := filepath.Join(work, "storage")
	if replace {
		os.RemoveAll(a.Cfg.Path("storage", slug))
	}
	lb := &localBackend{root: a.Cfg.Path("storage")}
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if len(parts) != 2 {
			return nil
		}
		if f, err := os.Open(p); err == nil {
			lb.put(ctx, slug, parts[0], parts[1], f, info.Size())
			f.Close()
		}
		return nil
	})
}
