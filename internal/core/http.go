package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Server exposes every operation over REST, the generic ops endpoint and MCP,
// serves the embedded web UI and proxies /fn/... to functions.
type Server struct {
	app    *App
	static fs.FS
	routes []route

	loginMu    sync.Mutex
	loginFails map[string][]time.Time
	certs      *certSource
}

type route struct {
	method string
	segs   []string
	op     *Op
}

func NewServer(a *App, static fs.FS) *Server {
	s := &Server{app: a, static: static, loginFails: map[string][]time.Time{}}
	for _, o := range a.Ops.All() {
		s.routes = append(s.routes, route{method: o.Method, segs: strings.Split(strings.Trim(o.Path, "/"), "/"), op: o})
	}
	return s
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	if s.app.Cfg.ServesTLS() {
		return s.serveTLS(ctx)
	}
	proxy := s.app.Cfg.TLSMode() == "proxy"
	if proxy && s.app.Cfg.hasCertFiles() {
		// HTTPS ends at the proxy, but direct Postgres connections still need
		// a certificate: use the proxy's certificate files, re-read on renewal.
		cs := newCertSource(s.app)
		s.certs = cs
		if err := cs.loadFiles(); err != nil {
			log.Printf("postgres TLS: %v (will retry)", err)
		}
		go s.app.syncPostgresTLS(ctx, cs)
	} else if err := s.app.ensureSelfSignedPostgresTLS(); err != nil {
		log.Printf("postgres TLS: %v", err)
	}
	srv := &http.Server{
		Addr:              s.app.Cfg.Listen,
		Handler:           s,
		ReadHeaderTimeout: 15 * time.Second,
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	ln, err := net.Listen("tcp", s.app.Cfg.Listen)
	if err != nil {
		return err
	}
	if proxy {
		log.Printf("IvaraBase %s listening on http://%s behind a proxy: panel %s, projects %s", Version, s.app.Cfg.Listen,
			s.app.Cfg.PublicURL, s.app.projectURL("<slug>"))
	} else {
		log.Printf("IvaraBase %s listening on http://%s  (UI, /api/v1, /mcp)", Version, s.app.Cfg.Listen)
	}
	log.Printf("docs and updates: %s", RepoURL)
	err = srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if s.app.isHTTPS(r) {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
	}
	// https://<slug>.<domain>/rest/v1/... is the same as /proj/<slug>/rest/v1/...
	if slug, ok := s.projectFromHost(r); ok {
		if r.URL.Path == "/" || r.URL.Path == "" {
			if _, err := s.app.project(slug); err != nil {
				supaError(w, 404, "Project not found", "")
				return
			}
			writeJSON(w, 200, map[string]any{"project": slug, "services": []string{"/rest/v1", "/auth/v1", "/storage/v1", "/functions/v1", "/realtime/v1"}})
			return
		}
		r.URL.Path = "/proj/" + slug + r.URL.Path
		r.URL.RawPath = ""
		s.handleProject(w, r)
		return
	}
	path := r.URL.Path
	switch {
	case path == "/healthz":
		if s.app.PG.Ping() != nil {
			http.Error(w, "postgres down", 503)
			return
		}
		w.Write([]byte("ok\n"))
	case strings.HasPrefix(path, "/proj/"):
		panelOriginSandbox(w)
		s.handleProject(w, r)
	case path == "/mcp":
		s.handleMCP(w, r)
	case strings.HasPrefix(path, "/mcp/ivb_") && !strings.Contains(path[5:], "/"):
		// for MCP clients that can't send headers (ChatGPT connectors): the
		// token is the last path segment. Treated exactly like a Bearer token.
		r.Header.Set("Authorization", "Bearer "+path[5:])
		s.handleMCP(w, r)
	case path == "/_tls/ask":
		s.handleTLSAsk(w, r)
	case strings.HasPrefix(path, "/fn/"):
		panelOriginSandbox(w)
		s.handleFunction(w, r)
	case path == "/api/v1/openapi.json":
		writeJSON(w, 200, s.app.Ops.OpenAPI(s.app.Cfg.PublicURL))
	case path == "/api/v1/auth/login":
		s.handleLogin(w, r)
	case path == "/api/v1/auth/logout":
		s.handleLogout(w, r)
	case strings.HasPrefix(path, "/api/"):
		s.handleAPI(w, r)
	default:
		s.handleStatic(w, r)
	}
}

// ---------------------------------------------------------------- auth

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func (s *Server) principal(r *http.Request) (*Principal, error) {
	if tok := bearer(r); tok != "" {
		return s.app.TokenPrincipal(tok)
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		if p, ok := s.app.SessionPrincipal(c.Value); ok {
			// cookie-authenticated writes must come from our own UI (CSRF guard)
			if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-Ivara") != "1" {
				return nil, &APIError{Status: 403, Code: "csrf", Message: "missing X-Ivara header on a cookie-authenticated request"}
			}
			return p, nil
		}
	}
	return nil, &APIError{Status: 401, Code: "unauthenticated", Message: "sign in or send Authorization: Bearer <token>",
		Hint: "create a token in the UI under Access tokens"}
}

// panelOriginSandbox: project APIs and functions reached through the panel's
// own origin (/proj/..., /fn/...) can return content we don't control - a
// function's HTML, an error page. A sandboxed response runs no script as the
// panel origin, so it can't drive the dashboard API with the admin's session.
// Projects that serve HTML on purpose do it from https://<slug>.<domain>.
func panelOriginSandbox(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", "sandbox")
}

func peerIP(r *http.Request) (string, net.IP) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return host, net.ParseIP(host)
}

// parseForwardedIP accepts "1.2.3.4", "1.2.3.4:5678", "[::1]:80" or "::1".
func parseForwardedIP(s string) net.IP {
	s = strings.TrimSpace(s)
	if ip := net.ParseIP(s); ip != nil {
		return ip
	}
	if h, _, err := net.SplitHostPort(s); err == nil {
		return net.ParseIP(h)
	}
	return net.ParseIP(strings.Trim(s, "[]"))
}

// clientIP is the visitor's address. Forwarding headers count only when the
// connection comes from a trusted proxy (loopback unless trusted_proxies says
// otherwise), so a client can't pick its own IP to dodge rate limits. The
// X-Forwarded-For chain is read right to left - nginx appends the address it
// saw - and the first hop that isn't a trusted proxy is the client.
func (a *App) clientIP(r *http.Request) string {
	host, peer := peerIP(r)
	if !a.Cfg.isTrustedProxy(peer) {
		return host
	}
	if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
		hops := strings.Split(strings.Join(xff, ","), ",")
		for i := len(hops) - 1; i >= 0; i-- {
			ip := parseForwardedIP(hops[i])
			if ip == nil {
				break // garbage: stop trusting the chain here
			}
			if i == 0 || !a.Cfg.isTrustedProxy(ip) {
				return ip.String()
			}
		}
	}
	if ip := parseForwardedIP(r.Header.Get("X-Real-IP")); ip != nil {
		return ip.String()
	}
	return host
}

// isHTTPS: the visitor's connection is HTTPS, either to us or to a trusted
// proxy that says so with X-Forwarded-Proto.
func (a *App) isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if _, peer := peerIP(r); !a.Cfg.isTrustedProxy(peer) {
		return false
	}
	proto, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ",")
	return strings.EqualFold(strings.TrimSpace(proto), "https")
}

// secureCookie: mark the session cookie Secure for HTTPS visitors, and for
// anyone reaching an https deployment other than through a local proxy (an
// SSH tunnel to 127.0.0.1:8787 is plain HTTP and needs a non-Secure cookie).
func (a *App) secureCookie(r *http.Request) bool {
	if a.isHTTPS(r) {
		return true
	}
	_, peer := peerIP(r)
	return strings.HasPrefix(a.Cfg.PublicURL, "https://") && !a.Cfg.isTrustedProxy(peer)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, &APIError{Status: 405, Code: "method_not_allowed", Message: "POST only"})
		return
	}
	ip := s.app.clientIP(r)
	s.loginMu.Lock()
	var recent []time.Time
	for _, t := range s.loginFails[ip] {
		if time.Since(t) < 10*time.Minute {
			recent = append(recent, t)
		}
	}
	s.loginFails[ip] = recent
	s.loginMu.Unlock()
	if len(recent) >= 10 {
		writeError(w, &APIError{Status: 429, Code: "too_many_attempts", Message: "too many failed sign-ins; wait 10 minutes"})
		return
	}
	var body struct{ Username, Password string }
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		writeError(w, errBad("invalid_json", "send {\"username\":..., \"password\":...}", ""))
		return
	}
	raw, err := s.app.Login(body.Username, body.Password, r.UserAgent())
	if err != nil {
		s.loginMu.Lock()
		s.loginFails[ip] = append(s.loginFails[ip], time.Now())
		s.loginMu.Unlock()
		writeError(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: raw, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: s.app.secureCookie(r),
		MaxAge: int(sessionTTL.Seconds())})
	writeJSON(w, 200, map[string]any{"ok": true, "username": body.Username})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.app.Logout(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- API

func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	p, err := s.principal(r)
	if err != nil {
		writeError(w, err)
		return
	}
	path := strings.TrimRight(r.URL.Path, "/")

	// operation catalog + generic ops endpoint
	if path == "/api/v1/ops" && r.Method == http.MethodGet {
		var out []map[string]any
		for _, o := range s.app.Ops.All() {
			if o.Level <= p.Level {
				out = append(out, o.Describe())
			}
		}
		writeJSON(w, 200, out)
		return
	}
	if strings.HasPrefix(path, "/api/v1/ops/") && r.Method == http.MethodPost {
		args, err := readArgs(r)
		if err != nil {
			writeError(w, err)
			return
		}
		s.respond(w, r, p, strings.TrimPrefix(path, "/api/v1/ops/"), args)
		return
	}
	if rest, ok := strings.CutPrefix(path, "/api/v1/projects/"); ok {
		if slug, action, ok := strings.Cut(rest, "/storage/"); ok && (action == "upload" || action == "download") {
			s.handleStorageFile(w, r, p, slug, action)
			return
		}
	}
	if m := strings.TrimPrefix(path, "/api/v1/backups/"); m != path && strings.HasSuffix(m, "/download") && r.Method == http.MethodGet {
		s.handleDownload(w, r, p, strings.TrimSuffix(m, "/download"))
		return
	}

	segs := strings.Split(strings.Trim(path, "/"), "/")
	var matched *route
	var vars map[string]string
	methodSeen := false
	best := -1
	for i := range s.routes {
		rt := &s.routes[i]
		v, ok := matchSegs(rt.segs, segs)
		if !ok {
			continue
		}
		methodSeen = true
		// prefer the most specific route: /backups/config beats /backups/{id}
		if rt.method == r.Method && len(rt.segs)-len(v) > best {
			matched, vars, best = rt, v, len(rt.segs)-len(v)
		}
	}
	if matched == nil {
		if methodSeen {
			writeError(w, &APIError{Status: 405, Code: "method_not_allowed", Message: r.Method + " is not supported on " + path})
			return
		}
		writeError(w, errNotFound("not_found", "no API route "+r.Method+" "+path, "GET /api/v1/ops lists every operation"))
		return
	}
	args := Args{}
	for k, vs := range r.URL.Query() {
		if len(vs) > 0 {
			args[k] = vs[0]
		}
	}
	if r.Method != http.MethodGet {
		body, err := readArgs(r)
		if err != nil {
			writeError(w, err)
			return
		}
		for k, v := range body {
			args[k] = v
		}
	}
	for k, v := range vars {
		args[k] = v
	}
	s.respond(w, r, p, matched.op.Name, args)
}

func matchSegs(pattern, path []string) (map[string]string, bool) {
	if len(pattern) != len(path) {
		return nil, false
	}
	vars := map[string]string{}
	for i, seg := range pattern {
		if strings.HasPrefix(seg, "{") {
			vars[seg[1:len(seg)-1]] = path[i]
			continue
		}
		if seg != path[i] {
			return nil, false
		}
	}
	return vars, true
}

func readArgs(r *http.Request) (Args, error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	args := Args{}
	if len(strings.TrimSpace(string(b))) == 0 {
		return args, nil
	}
	if err := json.Unmarshal(b, &args); err != nil {
		return nil, errBad("invalid_json", "request body must be a JSON object: "+err.Error(), "")
	}
	return args, nil
}

func (s *Server) respond(w http.ResponseWriter, r *http.Request, p *Principal, op string, args Args) {
	res, err := s.app.Ops.Execute(r.Context(), p, op, args)
	if err != nil {
		var ap *ApprovalPending
		if errors.As(err, &ap) {
			writeJSON(w, 202, map[string]any{"status": "approval_required", "approval_id": ap.ID, "op": ap.Op, "preview": ap.Preview,
				"message": "the owner must approve this in the IvaraBase UI (Approvals). Poll approvals.get with this id for the result."})
			return
		}
		writeError(w, err)
		return
	}
	writeJSON(w, 200, res)
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request, p *Principal, id string) {
	if p.Level < LevelWrite {
		writeError(w, errForbidden("downloading backups needs a write token", ""))
		return
	}
	var project string
	if err := s.app.Meta.QueryRow(`SELECT project FROM backups WHERE id=$1`, id).Scan(&project); err != nil {
		writeError(w, errNotFound("backup_not_found", "no backup "+id, ""))
		return
	}
	if !p.canProject(project) {
		writeError(w, errForbidden("not allowed for this project", ""))
		return
	}
	path, cleanup, err := s.app.fetchBundle(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	defer cleanup()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.ivbak"`, id))
	http.ServeFile(w, r, path)
}

// ---------------------------------------------------------------- functions

// /fn/{project}/{name}/{rest...}
func (s *Server) handleFunction(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/fn/"), "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		writeError(w, errNotFound("not_found", "use /fn/<project>/<function>", ""))
		return
	}
	fn, err := s.app.function(parts[0], parts[1])
	if err != nil {
		writeError(w, err)
		return
	}
	if !fn.Public {
		p, err := s.principal(r)
		if err != nil {
			writeError(w, &APIError{Status: 401, Code: "unauthenticated", Message: "this function is private; send Authorization: Bearer <token>",
				Hint: "or deploy it with public=true"})
			return
		}
		if !p.canProject(fn.Project) {
			writeError(w, errForbidden("token not allowed for this project", ""))
			return
		}
	}
	sub := "/"
	if len(parts) == 3 {
		sub = "/" + parts[2]
	}
	s.app.serveFunction(w, r, fn, sub)
}

// ---------------------------------------------------------------- static UI

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" {
		name = "index.html"
	}
	// real files (Vite build output); hashed assets never change
	if name != "index.html" {
		if st, err := fs.Stat(s.static, name); err == nil && !st.IsDir() {
			if strings.HasPrefix(name, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "public, max-age=3600")
			}
			http.FileServer(http.FS(s.static)).ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(name, "assets/") {
			http.NotFound(w, r)
			return
		}
	}
	// SPA: every other path gets index.html (client-side routing)
	b, err := fs.ReadFile(s.static, "index.html")
	if err != nil {
		http.Error(w, "UI not embedded", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self' data:; connect-src 'self'; frame-ancestors 'none'")
	w.Write(b)
}

// ---------------------------------------------------------------- JSON

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	ae := AsAPIError(err)
	if ae.Status >= 500 {
		log.Printf("error: %v", err)
	}
	writeJSON(w, ae.Status, map[string]any{"error": ae})
}
