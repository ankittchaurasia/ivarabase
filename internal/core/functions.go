package core

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Functions run as warm Deno processes: one per deployed function,
// started on first request, stopped after 10 idle minutes or on redeploy.
// Each process gets a clean environment (no host variables) containing only
// IVARA_* values and the project's secrets.

const fnRunnerTS = `// ivarabase function runner (generated)
// Supports both styles: "export default handler" and Supabase's "Deno.serve(handler)".
let captured: ((req: Request) => Response | Promise<Response>) | null = null;
const realServe = Deno.serve;
(Deno as any).serve = (...args: any[]) => {
  captured = args.find((a) => typeof a === "function") ?? args[0]?.handler ?? null;
  return { finished: new Promise(() => {}), shutdown: async () => {}, addr: { hostname: "127.0.0.1", port: 0 }, ref() {}, unref() {} };
};
const mod = await import(Deno.env.get("IVARA_FN_URL")!);
(Deno as any).serve = realServe;
const handler = mod.default ?? mod.handler ?? captured;
if (typeof handler !== "function") {
  console.error("a function must 'export default (req: Request) => Response' or call Deno.serve(handler)");
  Deno.exit(3);
}
realServe({
  hostname: "127.0.0.1",
  port: Number(Deno.env.get("IVARA_FN_PORT")),
  onListen() { console.log("IVARA_READY"); },
}, async (req) => {
  try {
    const out = await handler(req);
    if (out instanceof Response) return out;
    if (typeof out === "string") return new Response(out);
    return Response.json(out ?? null);
  } catch (e) {
    console.error(e instanceof Error ? (e.stack ?? e.message) : String(e));
    return Response.json({ error: String(e instanceof Error ? e.message : e) }, { status: 500 });
  }
});
`

const defaultFunctionCode = `// Invoke with supabase.functions.invoke("<name>", { body: { name: "you" } })
// Env: SUPABASE_URL, SUPABASE_ANON_KEY, SUPABASE_SERVICE_ROLE_KEY, SUPABASE_DB_URL + your secrets
export default async (req: Request): Promise<Response> => {
  const { name } = req.method === "POST" ? await req.json().catch(() => ({})) : {};
  console.log("hello called with", name);
  return Response.json({ message: "Hello " + (name ?? "world") + "!" });
};
`

var fnNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,40}$`)

type fnProc struct {
	cmd      *exec.Cmd
	port     int
	version  int
	lastUsed time.Time
	done     chan struct{}
	stderr   *ringBuf
}

type FnRuntime struct {
	app   *App
	mu    sync.Mutex
	procs map[string]*fnProc // project/name
	start sync.Map           // project/name -> *sync.Mutex (serialize cold starts)

	denoOnce sync.Once
	deno     string
	denoVer  string
}

func newFnRuntime(a *App) *FnRuntime { return &FnRuntime{app: a, procs: map[string]*fnProc{}} }

func (f *FnRuntime) denoPath() string {
	f.denoOnce.Do(func() {
		cands := []string{f.app.Cfg.DenoPath}
		if p, err := exec.LookPath("deno"); err == nil {
			cands = append(cands, p)
		}
		if home, err := os.UserHomeDir(); err == nil {
			cands = append(cands, filepath.Join(home, ".deno", "bin", "deno"))
		}
		cands = append(cands, "/opt/homebrew/bin/deno", "/usr/local/bin/deno", "/usr/bin/deno")
		for _, c := range cands {
			if c != "" && fileExists(c) {
				f.deno = c
				if out, err := exec.Command(c, "--version").Output(); err == nil {
					f.denoVer = strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
				}
				return
			}
		}
	})
	return f.deno
}

func (f *FnRuntime) denoVersion() map[string]any {
	p := f.denoPath()
	return map[string]any{"path": p, "version": f.denoVer, "available": p != ""}
}

func fnKey(project, name string) string { return project + "/" + name }

func (f *FnRuntime) Stop(project, name string) {
	f.mu.Lock()
	p := f.procs[fnKey(project, name)]
	delete(f.procs, fnKey(project, name))
	f.mu.Unlock()
	if p != nil {
		killProc(p)
	}
}

func (f *FnRuntime) StopProject(project string) {
	f.mu.Lock()
	var victims []*fnProc
	for k, p := range f.procs {
		if strings.HasPrefix(k, project+"/") {
			victims = append(victims, p)
			delete(f.procs, k)
		}
	}
	f.mu.Unlock()
	for _, p := range victims {
		killProc(p)
	}
}

func (f *FnRuntime) StopAll() {
	f.mu.Lock()
	all := f.procs
	f.procs = map[string]*fnProc{}
	f.mu.Unlock()
	for _, p := range all {
		killProc(p)
	}
}

func killProc(p *fnProc) {
	if p.cmd.Process != nil {
		p.cmd.Process.Kill()
	}
	select {
	case <-p.done:
	case <-time.After(3 * time.Second):
	}
}

func (f *FnRuntime) reaper(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f.mu.Lock()
			var idle []*fnProc
			for k, p := range f.procs {
				if time.Since(p.lastUsed) > 10*time.Minute {
					idle = append(idle, p)
					delete(f.procs, k)
				}
			}
			f.mu.Unlock()
			for _, p := range idle {
				killProc(p)
			}
		}
	}
}

type fnRow struct {
	Project, Name, Code string
	Files               map[string]string // path -> source; Code is the entrypoint's source
	Entrypoint          string
	Public              bool // legacy /fn route: no IvaraBase token needed
	VerifyJWT           bool // /functions/v1 route: caller must send a project JWT
	Schedule            string
	TimeoutS, MemoryMB  int
	Version             int
	UpdatedAt           time.Time
}

func (a *App) function(project, name string) (*fnRow, error) {
	var r fnRow
	var files []byte
	err := a.Meta.QueryRow(`SELECT project,name,code,files,entrypoint,public,verify_jwt,schedule,timeout_s,memory_mb,version,updated_at FROM functions WHERE project=$1 AND name=$2`,
		project, name).Scan(&r.Project, &r.Name, &r.Code, &files, &r.Entrypoint, &r.Public, &r.VerifyJWT, &r.Schedule, &r.TimeoutS, &r.MemoryMB, &r.Version, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound("function_not_found", fmt.Sprintf("function %q not found in %s", name, project), "call functions.list")
	}
	if err != nil {
		return nil, err
	}
	json.Unmarshal(files, &r.Files)
	if len(r.Files) == 0 {
		r.Files = map[string]string{r.Entrypoint: r.Code}
	}
	return &r, nil
}

func (a *App) fnDir(project, name string) string { return a.Cfg.Path("functions", project, name) }

// fnFile is the entrypoint on disk.
func (a *App) fnFile(project, name string) string {
	if r, err := a.function(project, name); err == nil && r.Entrypoint != "" {
		return filepath.Join(a.fnDir(project, name), filepath.FromSlash(r.Entrypoint))
	}
	return filepath.Join(a.fnDir(project, name), "index.ts")
}

var fnPathRe = regexp.MustCompile(`^[A-Za-z0-9_\-.]+(/[A-Za-z0-9_\-.]+)*$`)

// validateFnFiles keeps every file inside the function's own directory.
func validateFnFiles(files map[string]string, entry string) error {
	if len(files) == 0 {
		return errBad("missing_parameter", "a function needs at least one file", "")
	}
	if len(files) > 200 {
		return errBad("too_many_files", "a function can have at most 200 files", "")
	}
	total := 0
	for p, src := range files {
		if !fnPathRe.MatchString(p) || strings.Contains(p, "..") || len(p) > 200 {
			return errBad("invalid_file_path", "invalid file path "+p, "use relative paths like index.ts or lib/db.ts")
		}
		total += len(src)
	}
	if total > 10<<20 {
		return errBad("too_large", "function sources are limited to 10 MB", "")
	}
	if _, ok := files[entry]; !ok {
		return errBad("missing_entrypoint", "entrypoint "+entry+" is not among the files", "")
	}
	return nil
}

func (a *App) writeFnFiles(r *fnRow) error {
	dir := a.fnDir(r.Project, r.Name)
	os.RemoveAll(dir)
	for p, src := range r.Files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if !strings.HasPrefix(full, dir+string(filepath.Separator)) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
			return err
		}
	}
	return os.WriteFile(a.Cfg.Path("functions", "_runner.ts"), []byte(fnRunnerTS), 0o644)
}

// ensure returns a running process for the function, cold-starting it if needed.
func (f *FnRuntime) ensure(r *fnRow) (*fnProc, error) {
	key := fnKey(r.Project, r.Name)
	f.mu.Lock()
	if p, ok := f.procs[key]; ok && p.version == r.Version {
		select {
		case <-p.done:
			delete(f.procs, key)
		default:
			p.lastUsed = time.Now()
			f.mu.Unlock()
			return p, nil
		}
	}
	f.mu.Unlock()

	lk, _ := f.start.LoadOrStore(key, &sync.Mutex{})
	lk.(*sync.Mutex).Lock()
	defer lk.(*sync.Mutex).Unlock()
	f.mu.Lock()
	if p, ok := f.procs[key]; ok && p.version == r.Version {
		f.mu.Unlock()
		return p, nil
	}
	old := f.procs[key]
	delete(f.procs, key)
	f.mu.Unlock()
	if old != nil {
		killProc(old)
	}

	deno := f.denoPath()
	if deno == "" {
		return nil, &APIError{Status: 503, Code: "deno_missing", Message: "Deno is not installed on the server",
			Hint: "install Deno or set deno_path in config.json"}
	}
	if err := f.app.writeFnFiles(r); err != nil {
		return nil, err
	}
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	env, err := f.app.fnEnv(r.Project)
	if err != nil {
		return nil, err
	}
	fnDir := f.app.fnDir(r.Project, r.Name)
	entry := filepath.Join(fnDir, filepath.FromSlash(r.Entrypoint))
	env = append(env,
		fmt.Sprintf("IVARA_FN_PORT=%d", port),
		"IVARA_FN_URL="+(&url.URL{Scheme: "file", Path: entry}).String(),
		"IVARA_FUNCTION="+r.Name,
		"DENO_DIR="+f.app.Cfg.Path("functions", ".deno-cache"),
		"DENO_NO_UPDATE_CHECK=1", "NO_COLOR=1",
	)
	if home, _ := os.UserHomeDir(); home != "" {
		env = append(env, "HOME="+home)
	}
	cmd := exec.Command(deno, "run", "--quiet", "--no-prompt",
		"--allow-net", "--allow-env", "--allow-read="+fnDir+","+f.app.Cfg.Path("functions", "_runner.ts"),
		fmt.Sprintf("--v8-flags=--max-old-space-size=%d", r.MemoryMB),
		f.app.Cfg.Path("functions", "_runner.ts"))
	cmd.Env = env
	cmd.Dir = fnDir
	stdout, _ := cmd.StdoutPipe()
	ring := &ringBuf{max: 16 * 1024}
	cmd.Stderr = io.MultiWriter(ring, &lineWriter{fn: func(line string) { f.console(r.Project, r.Name, "stderr", line) }})
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &fnProc{cmd: cmd, port: port, version: r.Version, lastUsed: time.Now(), done: make(chan struct{}), stderr: ring}
	ready := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(stdout)
		once := sync.Once{}
		for sc.Scan() {
			line := sc.Text()
			if strings.Contains(line, "IVARA_READY") {
				once.Do(func() { close(ready) })
				continue
			}
			ring.Write([]byte(line + "\n"))
			f.console(r.Project, r.Name, "stdout", line)
		}
	}()
	go func() { cmd.Wait(); close(p.done) }()

	select {
	case <-ready:
	case <-p.done:
		return nil, &APIError{Status: 502, Code: "function_crashed", Message: "the function exited during startup",
			Hint: "check the code compiles; stderr: " + tail(ring.String(), 800)}
	case <-time.After(45 * time.Second):
		killProc(p)
		return nil, &APIError{Status: 504, Code: "function_start_timeout", Message: "the function did not start within 45s", Hint: tail(ring.String(), 800)}
	}
	f.mu.Lock()
	f.procs[key] = p
	f.mu.Unlock()
	return p, nil
}

func (a *App) fnEnv(project string) ([]string, error) {
	p, err := a.project(project)
	if err != nil {
		return nil, err
	}
	pw, err := a.projectPassword(p)
	if err != nil {
		return nil, err
	}
	conn := a.connectionInfo(p, pw)
	env := []string{
		"IVARA_PROJECT=" + project,
		"IVARA_URL=" + a.Cfg.PublicURL,
		"IVARA_DB_URL=" + localDBURL(conn),
		"SUPABASE_DB_URL=" + localDBURL(conn),
		"SUPABASE_URL=" + a.localProjectURL(project), // loopback: no DNS or TLS round trip
		"IVARA_PUBLIC_PROJECT_URL=" + a.projectURL(project),
		"IVARA_PROJECT_URL=" + a.projectURL(project),
		"PATH=/usr/bin:/bin",
	}
	if st, err := a.projectAPIState(project); err == nil {
		anon, svc := apiKey(st.Secret, project, roleAnon), apiKey(st.Secret, project, roleService)
		env = append(env, "SUPABASE_ANON_KEY="+anon, "SUPABASE_SERVICE_ROLE_KEY="+svc, "IVARA_ANON_KEY="+anon, "IVARA_SERVICE_ROLE_KEY="+svc)
	}
	rows, err := a.Meta.Query(`SELECT name, value_enc FROM secrets WHERE project=$1`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var n, enc string
		rows.Scan(&n, &enc)
		if v, err := unseal(a.masterKey, enc); err == nil {
			env = append(env, n+"="+v)
		}
	}
	return env, nil
}

// Serve proxies an HTTP request to the function. Used by /fn/... and functions.invoke.
func (a *App) serveFunction(w http.ResponseWriter, r *http.Request, fn *fnRow, subpath string, keepAuth ...bool) {
	passAuth := len(keepAuth) > 0 && keepAuth[0]
	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w, status: 200}
	logErr := ""
	defer func() {
		a.Meta.Exec(`INSERT INTO function_logs(project,name,status,ms,error) VALUES ($1,$2,$3,$4,$5)`,
			fn.Project, fn.Name, rec.status, time.Since(start).Milliseconds(), logErr)
	}()
	p, err := a.Fn.ensure(fn)
	if err != nil {
		ae := AsAPIError(err)
		rec.status = ae.Status
		logErr = ae.Message + " " + ae.Hint
		writeError(w, ae)
		return
	}
	target, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", p.port))
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		rec.status = 502
		logErr = err.Error() + " " + tail(p.stderr.String(), 500)
		writeError(w, &APIError{Status: 502, Code: "function_error", Message: err.Error(), Hint: tail(p.stderr.String(), 500)})
	}
	before := p.stderr.Len()
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(fn.TimeoutS)*time.Second)
	defer cancel()
	r2 := r.Clone(ctx)
	r2.URL.Path = "/" + strings.TrimPrefix(subpath, "/")
	r2.Host = target.Host
	// Tell the function who the visitor really is (not nginx, not a spoofed
	// header): x-forwarded-for / x-real-ip carry the trusted client address.
	ip := a.clientIP(r)
	r2.Header.Set("X-Forwarded-For", ip)
	r2.Header.Set("X-Real-IP", ip)
	if a.isHTTPS(r) {
		r2.Header.Set("X-Forwarded-Proto", "https")
	} else {
		r2.Header.Set("X-Forwarded-Proto", "http")
	}
	if !passAuth {
		r2.Header.Del("Authorization") // an IvaraBase admin token must never reach user code
	} else {
		proxy.ModifyResponse = func(res *http.Response) error {
			for k := range res.Header {
				if strings.HasPrefix(strings.ToLower(k), "access-control-") {
					res.Header.Del(k) // the gateway already sent CORS headers
				}
			}
			return nil
		}
	}
	r2.Header.Del("Cookie")
	proxy.ServeHTTP(rec, r2)
	if ctx.Err() == context.DeadlineExceeded {
		logErr = fmt.Sprintf("timed out after %ds", fn.TimeoutS)
	}
	if rec.status >= 500 && logErr == "" {
		logErr = tail(p.stderr.Since(before), 1000)
	}
	p.lastUsed = time.Now()
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(c int) {
	if !s.wrote {
		s.status, s.wrote = c, true
	}
	s.ResponseWriter.WriteHeader(c)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

type ringBuf struct {
	mu  sync.Mutex
	buf []byte
	max int
	off int // total bytes ever written
}

func (r *ringBuf) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	r.off += len(p)
	if len(r.buf) > r.max {
		r.buf = r.buf[len(r.buf)-r.max:]
	}
	return len(p), nil
}

func (r *ringBuf) String() string { r.mu.Lock(); defer r.mu.Unlock(); return string(r.buf) }
func (r *ringBuf) Len() int       { r.mu.Lock(); defer r.mu.Unlock(); return r.off }

func (r *ringBuf) Since(off int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.off - off
	if n <= 0 {
		return ""
	}
	if n > len(r.buf) {
		n = len(r.buf)
	}
	return string(r.buf[len(r.buf)-n:])
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// ---------------------------------------------------------------- ops

func registerFunctionOps(r *Registry) {
	projectParam := Param{Name: "project", Type: "string", Required: true, Desc: "project slug"}
	nameParam := Param{Name: "name", Type: "string", Required: true, Desc: "function name: a-z 0-9 _ -"}

	fnURL := func(a *App, project, name string) string { return a.projectURL(project) + "/functions/v1/" + name }

	r.add(&Op{Name: "functions.list", Tag: "functions", Method: "GET", Path: "/api/v1/projects/{project}/functions", Level: LevelRead,
		Summary: "List functions with 24h call stats and schedules",
		Params:  []Param{projectParam},
		Run: func(c *Ctx, a Args) (any, error) {
			rows, err := c.App.Meta.Query(`SELECT f.name, f.verify_jwt, f.schedule, f.last_run_at, f.timeout_s, f.memory_mb, f.version, f.updated_at, (SELECT count(*) FROM jsonb_object_keys(f.files))::int,
				(SELECT count(*) FROM function_logs l WHERE l.project=f.project AND l.name=f.name AND l.at > now()-interval '24 hours'),
				(SELECT count(*) FROM function_logs l WHERE l.project=f.project AND l.name=f.name AND l.at > now()-interval '24 hours' AND l.status>=500),
				(SELECT coalesce(avg(ms),0)::int FROM function_logs l WHERE l.project=f.project AND l.name=f.name AND l.at > now()-interval '24 hours')
				FROM functions f WHERE f.project=$1 ORDER BY f.name`, a.Str("project"))
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			out := []map[string]any{}
			for rows.Next() {
				var name, sched string
				var verify bool
				var last sql.NullTime
				var to, mem, ver, files, calls, errs, avg int
				var up time.Time
				rows.Scan(&name, &verify, &sched, &last, &to, &mem, &ver, &up, &files, &calls, &errs, &avg)
				c.App.Fn.mu.Lock()
				_, warm := c.App.Fn.procs[fnKey(a.Str("project"), name)]
				c.App.Fn.mu.Unlock()
				m := map[string]any{"name": name, "verify_jwt": verify, "schedule": sched, "last_run_at": nt(last), "timeout_s": to, "memory_mb": mem,
					"version": ver, "updated_at": up, "files": max(files, 1), "calls_24h": calls, "errors_24h": errs, "avg_ms_24h": avg, "warm": warm,
					"url": fnURL(c.App, a.Str("project"), name)}
				if sched != "" {
					if cs, err := parseCron(sched); err == nil {
						m["next_run_at"] = cs.next(time.Now())
					}
				}
				out = append(out, m)
			}
			return out, rows.Err()
		}})

	r.add(&Op{Name: "functions.get", Tag: "functions", Method: "GET", Path: "/api/v1/projects/{project}/functions/{name}", Level: LevelRead,
		Summary: "Get a function's files and settings",
		Params:  []Param{projectParam, nameParam},
		Run: func(c *Ctx, a Args) (any, error) {
			f, err := c.App.function(a.Str("project"), a.Str("name"))
			if err != nil {
				return nil, err
			}
			out := map[string]any{"name": f.Name, "files": f.Files, "entrypoint": f.Entrypoint, "code": f.Code, "verify_jwt": f.VerifyJWT,
				"schedule": f.Schedule, "timeout_s": f.TimeoutS, "memory_mb": f.MemoryMB, "version": f.Version, "updated_at": f.UpdatedAt,
				"url": fnURL(c.App, f.Project, f.Name), "legacy_url": c.App.Cfg.PublicURL + "/fn/" + f.Project + "/" + f.Name}
			if f.Schedule != "" {
				if cs, err := parseCron(f.Schedule); err == nil {
					out["next_run_at"] = cs.next(time.Now())
				}
			}
			return out, nil
		}})

	r.add(&Op{Name: "functions.template", Tag: "functions", Method: "GET", Path: "/api/v1/functions/template", Level: LevelRead,
		Summary: "Starter code for a new function",
		Run: func(c *Ctx, a Args) (any, error) {
			return map[string]any{"code": defaultFunctionCode, "entrypoint": "index.ts"}, nil
		}})

	r.add(&Op{Name: "functions.deploy", Tag: "functions", Method: "PUT", Path: "/api/v1/projects/{project}/functions/{name}", Level: LevelWrite,
		Summary:     "Create or update a function (TypeScript/JavaScript for Deno); live on the next request",
		Description: "Send `files` ({\"index.ts\": \"...\", \"lib/db.ts\": \"...\"}) or a single `code`. The entrypoint must `export default (req: Request) => Response` or call Deno.serve(handler). Called at <project url>/functions/v1/<name> (supabase.functions.invoke). Env: SUPABASE_URL, SUPABASE_ANON_KEY, SUPABASE_SERVICE_ROLE_KEY, SUPABASE_DB_URL and the project's secrets.",
		Params: []Param{projectParam, nameParam,
			{Name: "files", Type: "object", Desc: "path -> source for every file of the function"},
			{Name: "code", Type: "string", Format: "typescript", Desc: "single-file shortcut (becomes the entrypoint)"},
			{Name: "entrypoint", Type: "string", Desc: "default index.ts"},
			{Name: "verify_jwt", Type: "boolean", Desc: "default true: callers must send the project's anon key or a user's token; false for webhooks"},
			{Name: "public", Type: "boolean", Desc: "deprecated alias: public=true means verify_jwt=false"},
			{Name: "schedule", Type: "string", Desc: "cron (UTC, 5 fields) to run it automatically, e.g. */15 * * * *; empty string removes it"},
			{Name: "timeout_s", Type: "integer", Desc: "5-300, default 30"},
			{Name: "memory_mb", Type: "integer", Desc: "64-1024, default 128"}},
		Run: func(c *Ctx, a Args) (any, error) {
			project, name := a.Str("project"), strings.ToLower(a.Str("name"))
			if _, err := c.App.project(project); err != nil {
				return nil, err
			}
			if !fnNameRe.MatchString(name) {
				return nil, errBad("invalid_name", "function names are 2-41 chars: a-z 0-9 _ - starting with a letter", "")
			}
			cur, _ := c.App.function(project, name)
			entry := a.Str("entrypoint")
			if entry == "" {
				entry = "index.ts"
				if cur != nil {
					entry = cur.Entrypoint
				}
			}
			files := map[string]string{}
			if fo := a.Obj("files"); fo != nil {
				for k, v := range fo {
					sv, ok := v.(string)
					if !ok {
						return nil, errBad("invalid_parameter", "files values must be strings (file contents)", "")
					}
					files[strings.TrimPrefix(k, "./")] = sv
				}
				if _, ok := files[entry]; !ok && a.Str("entrypoint") == "" {
					for _, cand := range []string{"index.ts", "index.js", "main.ts", "mod.ts"} {
						if _, ok := files[cand]; ok {
							entry = cand
							break
						}
					}
				}
			} else if code := a.Str("code"); code != "" {
				if cur != nil {
					for k, v := range cur.Files {
						files[k] = v
					}
				}
				files[entry] = code
			} else {
				return nil, errBad("missing_parameter", "send files or code", "")
			}
			if err := validateFnFiles(files, entry); err != nil {
				return nil, err
			}
			verify, sched, timeout, mem := true, "", 30, 128
			if cur != nil {
				verify, sched, timeout, mem = cur.VerifyJWT, cur.Schedule, cur.TimeoutS, cur.MemoryMB
			}
			if _, ok := a["public"]; ok {
				verify = !a.Bool("public", false)
			}
			if _, ok := a["verify_jwt"]; ok {
				verify = a.Bool("verify_jwt", true)
			}
			if _, ok := a["schedule"]; ok {
				sched = strings.TrimSpace(a.Str("schedule"))
				if sched != "" {
					if _, err := parseCron(sched); err != nil {
						return nil, errBad("invalid_schedule", err.Error(), "5 fields: minute hour day-of-month month day-of-week, e.g. 0 3 * * *")
					}
				}
			}
			timeout = clamp(a.Int("timeout_s", timeout), 5, 300)
			mem = clamp(a.Int("memory_mb", mem), 64, 1024)
			fj, _ := json.Marshal(files)
			var version int
			err := c.App.Meta.QueryRow(`INSERT INTO functions(project,name,code,files,entrypoint,public,verify_jwt,schedule,timeout_s,memory_mb) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
				ON CONFLICT (project,name) DO UPDATE SET code=$3, files=$4, entrypoint=$5, public=$6, verify_jwt=$7, schedule=$8, timeout_s=$9, memory_mb=$10,
				version=functions.version+1, updated_at=now()
				RETURNING version`, project, name, files[entry], fj, entry, !verify, verify, sched, timeout, mem).Scan(&version)
			if err != nil {
				return nil, err
			}
			c.App.Fn.Stop(project, name)
			out := map[string]any{"name": name, "version": version, "files": len(files), "entrypoint": entry, "verify_jwt": verify,
				"schedule": sched, "timeout_s": timeout, "memory_mb": mem, "url": fnURL(c.App, project, name)}
			if sched != "" {
				cs, _ := parseCron(sched)
				out["next_run_at"] = cs.next(time.Now())
			}
			return out, nil
		}})

	r.add(&Op{Name: "functions.console", Tag: "functions", Method: "GET", Path: "/api/v1/projects/{project}/functions/{name}/console", Level: LevelRead,
		Summary: "console.log / console.error output of a function (newest first)",
		Params:  []Param{projectParam, nameParam, {Name: "limit", Type: "integer", Default: 200}},
		Run: func(c *Ctx, a Args) (any, error) {
			c.App.Fn.flushConsole()
			rows, err := c.App.Meta.Query(fmt.Sprintf(`SELECT id, at, stream, line FROM function_console WHERE project=$1 AND name=$2 ORDER BY id DESC LIMIT %d`,
				clamp(a.Int("limit", 200), 1, 2000)), a.Str("project"), a.Str("name"))
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			out := []map[string]any{}
			for rows.Next() {
				var id int64
				var at time.Time
				var stream, line string
				rows.Scan(&id, &at, &stream, &line)
				out = append(out, map[string]any{"id": id, "at": at, "stream": stream, "line": line})
			}
			return out, rows.Err()
		}})

	r.add(&Op{Name: "functions.delete", Tag: "functions", Method: "DELETE", Path: "/api/v1/projects/{project}/functions/{name}", Level: LevelWrite,
		Destructive: true,
		Summary:     "Delete a function",
		Params:      []Param{projectParam, nameParam},
		Preview: func(c *Ctx, a Args) (any, error) {
			f, err := c.App.function(a.Str("project"), a.Str("name"))
			if err != nil {
				return nil, err
			}
			return map[string]any{"function": f.Name, "version": f.Version, "files": len(f.Files), "schedule": f.Schedule}, nil
		},
		Run: func(c *Ctx, a Args) (any, error) {
			project, name := a.Str("project"), a.Str("name")
			if _, err := c.App.function(project, name); err != nil {
				return nil, err
			}
			c.App.Fn.Stop(project, name)
			c.App.Meta.Exec(`DELETE FROM functions WHERE project=$1 AND name=$2`, project, name)
			os.RemoveAll(filepath.Dir(c.App.fnFile(project, name)))
			return map[string]any{"deleted": name}, nil
		}})

	r.add(&Op{Name: "functions.invoke", Tag: "functions", Method: "POST", Path: "/api/v1/projects/{project}/functions/{name}/invoke", Level: LevelWrite,
		Summary: "Call a function and return its status, headers and body (for testing)",
		Params: []Param{projectParam, nameParam,
			{Name: "method", Type: "string", Default: "POST", Enum: []string{"GET", "POST", "PUT", "PATCH", "DELETE"}},
			{Name: "path", Type: "string", Default: "/", Desc: "sub-path and query, e.g. /orders?limit=5"},
			{Name: "body", Type: "any", Desc: "JSON body (objects are sent as application/json)"},
			{Name: "headers", Type: "object"}},
		Run: func(c *Ctx, a Args) (any, error) {
			f, err := c.App.function(a.Str("project"), a.Str("name"))
			if err != nil {
				return nil, err
			}
			var body io.Reader
			ctype := ""
			switch b := a["body"].(type) {
			case nil:
			case string:
				body = strings.NewReader(b)
				ctype = "text/plain"
			default:
				j, _ := json.Marshal(b)
				body = bytes.NewReader(j)
				ctype = "application/json"
			}
			path := a.Str("path")
			if !strings.HasPrefix(path, "/") {
				path = "/" + path
			}
			req, err := http.NewRequestWithContext(c, a.Str("method"), "http://function"+path, body)
			if err != nil {
				return nil, errBad("invalid_parameter", err.Error(), "")
			}
			if ctype != "" {
				req.Header.Set("Content-Type", ctype)
			}
			for k, v := range a.Obj("headers") {
				req.Header.Set(k, fmt.Sprint(v))
			}
			rec := newMemResponse()
			start := time.Now()
			u, _ := url.Parse(path)
			c.App.serveFunction(rec, req, f, u.Path)
			out := map[string]any{"status": rec.status, "headers": flatHeaders(rec.header), "ms": time.Since(start).Milliseconds()}
			raw := rec.body.Bytes()
			var j any
			if strings.Contains(rec.header.Get("Content-Type"), "json") && json.Unmarshal(raw, &j) == nil {
				out["body"] = j
			} else {
				if len(raw) > 256*1024 {
					raw = raw[:256*1024]
					out["truncated"] = true
				}
				out["body"] = string(raw)
			}
			return out, nil
		}})

	r.add(&Op{Name: "functions.logs", Tag: "functions", Method: "GET", Path: "/api/v1/projects/{project}/function-logs", Level: LevelRead,
		Summary: "Recent invocations: status, duration, error output",
		Params:  []Param{projectParam, {Name: "name", Type: "string", Desc: "only this function"}, {Name: "errors_only", Type: "boolean", Default: false}, {Name: "limit", Type: "integer", Default: 100}},
		Run: func(c *Ctx, a Args) (any, error) {
			q := `SELECT id,name,at,status,ms,error FROM function_logs WHERE project=$1`
			args := []any{a.Str("project")}
			if n := a.Str("name"); n != "" {
				args = append(args, n)
				q += fmt.Sprintf(` AND name=$%d`, len(args))
			}
			if a.Bool("errors_only", false) {
				q += ` AND status >= 400`
			}
			q += fmt.Sprintf(` ORDER BY at DESC LIMIT %d`, clamp(a.Int("limit", 100), 1, 1000))
			rows, err := c.App.Meta.Query(q, args...)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			out := []map[string]any{}
			for rows.Next() {
				var id int64
				var name, e string
				var at time.Time
				var st, ms int
				rows.Scan(&id, &name, &at, &st, &ms, &e)
				out = append(out, map[string]any{"id": id, "name": name, "at": at, "status": st, "ms": ms, "error": e})
			}
			return out, rows.Err()
		}})

	// secrets
	r.add(&Op{Name: "secrets.list", Tag: "functions", Method: "GET", Path: "/api/v1/projects/{project}/secrets", Level: LevelRead,
		Summary: "List secret names (values are never returned)",
		Params:  []Param{projectParam},
		Run: func(c *Ctx, a Args) (any, error) {
			rows, err := c.App.Meta.Query(`SELECT name, updated_at FROM secrets WHERE project=$1 ORDER BY name`, a.Str("project"))
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			out := []map[string]any{}
			for rows.Next() {
				var n string
				var t time.Time
				rows.Scan(&n, &t)
				out = append(out, map[string]any{"name": n, "updated_at": t})
			}
			return out, rows.Err()
		}})

	secretName := regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,63}$`)
	r.add(&Op{Name: "secrets.set", Tag: "functions", Method: "PUT", Path: "/api/v1/projects/{project}/secrets/{name}", Level: LevelWrite,
		Summary: "Create or replace a secret (exposed to functions as an env var)",
		Params: []Param{projectParam, {Name: "name", Type: "string", Required: true, Desc: "UPPER_SNAKE_CASE"},
			{Name: "value", Type: "string", Required: true, Secret: true}},
		Run: func(c *Ctx, a Args) (any, error) {
			if _, err := c.App.project(a.Str("project")); err != nil {
				return nil, err
			}
			n := a.Str("name")
			if !secretName.MatchString(n) || strings.HasPrefix(n, "IVARA_") || strings.HasPrefix(n, "DENO_") || n == "PATH" || n == "HOME" {
				return nil, errBad("invalid_name", "secret names are UPPER_SNAKE_CASE and may not start with IVARA_ or DENO_", "")
			}
			_, err := c.App.Meta.Exec(`INSERT INTO secrets(project,name,value_enc) VALUES ($1,$2,$3)
				ON CONFLICT (project,name) DO UPDATE SET value_enc=$3, updated_at=now()`, a.Str("project"), n, seal(c.App.masterKey, a.Str("value")))
			if err != nil {
				return nil, err
			}
			c.App.Fn.StopProject(a.Str("project")) // restart with the new env
			return map[string]any{"name": n, "set": true}, nil
		}})

	r.add(&Op{Name: "secrets.delete", Tag: "functions", Method: "DELETE", Path: "/api/v1/projects/{project}/secrets/{name}", Level: LevelWrite,
		Summary: "Delete a secret",
		Params:  []Param{projectParam, {Name: "name", Type: "string", Required: true}},
		Run: func(c *Ctx, a Args) (any, error) {
			res, err := c.App.Meta.Exec(`DELETE FROM secrets WHERE project=$1 AND name=$2`, a.Str("project"), a.Str("name"))
			if err != nil {
				return nil, err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return nil, errNotFound("secret_not_found", "no secret "+a.Str("name"), "")
			}
			c.App.Fn.StopProject(a.Str("project"))
			return map[string]any{"deleted": a.Str("name")}, nil
		}})
}

// memResponse captures a response in memory (functions.invoke).
type memResponse struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func newMemResponse() *memResponse { return &memResponse{header: http.Header{}, status: 200} }

func (m *memResponse) Header() http.Header         { return m.header }
func (m *memResponse) Write(b []byte) (int, error) { return m.body.Write(b) }
func (m *memResponse) WriteHeader(s int)           { m.status = s }

func flatHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	for k, v := range h {
		out[k] = strings.Join(v, ", ")
	}
	return out
}

// localDBURL points a project's connection string at the local cluster
// (functions run on the same machine, no TLS needed over loopback).
func localDBURL(conn map[string]any) string {
	return fmt.Sprintf("postgresql://%s:%s@127.0.0.1:%v/%s?sslmode=disable", conn["user"], conn["password"], conn["port"], conn["database"])
}

// localProjectURL reaches a project through the loopback listener, for code
// running on this machine (functions). Like Supabase's internal URL.
func (a *App) localProjectURL(slug string) string {
	port := "8787"
	if i := strings.LastIndex(a.Cfg.Listen, ":"); i >= 0 {
		port = a.Cfg.Listen[i+1:]
	}
	return "http://127.0.0.1:" + port + "/proj/" + slug
}
