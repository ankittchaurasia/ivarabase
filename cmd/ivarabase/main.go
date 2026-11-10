// Command ivarabase is the server, the CLI and the MCP stdio bridge in one binary.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"ivarabase/internal/core"
	"ivarabase/web"
)

const usage = `IvaraBase - self-hosted Postgres platform, operable by humans and AI agents.
Docs, releases and issues: https://github.com/ankittchaurasia/ivarabase

Server (runs on the machine that hosts the data):
  ivarabase serve [--data DIR] [--listen ADDR] [--public-url URL]
  ivarabase init  [--data DIR] [--user NAME] [--password PW]
  ivarabase token create --name NAME [--level read|write|admin] [--projects a,b] [--allow-destructive] [--data DIR]
  ivarabase reset-password [--user NAME] [--data DIR]
  ivarabase dr restore --from DIR | --s3-endpoint URL --s3-bucket B [--s3-prefix P] [--s3-region R]
                       [--s3-access-key K --s3-secret-key S] --recovery-key KEY [--projects a,b] [--data DIR]

Running it for real (VPS or always-on Mac):
  ivarabase doctor                      check Postgres, PostgREST, Deno, DNS and ports
  ivarabase proxy-config [--server caddy]  nginx (or Caddy) config for running behind a proxy
  ivarabase config show | set KEY VALUE e.g. config set domain base.example.com
  ivarabase env                         IVARA_* settings (environment or <data>/ivarabase.env override config.json)
  ivarabase healthcheck                 exit 0 when the local server is healthy (Docker HEALTHCHECK)
  sudo ivarabase service install [--domain base.example.com --email you@example.com]
  sudo ivarabase service install --domain base.example.com --proxy [--cert-file F --key-file K]
                                        behind nginx/Caddy/Traefik: never binds 80/443
  ivarabase service status|logs|restart|uninstall

Config as code (ivarabase.yaml + migrations/ + functions/ in your repo):
  ivarabase export --project shop [--dir .]   write the project's current state as files
  ivarabase plan [-f ivarabase.yaml]          show what would change
  ivarabase apply [--yes] [--prune]           make the project match the files
  ivarabase db types --project shop --out database.types.ts

Client (talks to a running server; set IVARA_URL / IVARA_TOKEN or run login):
  ivarabase login --url http://127.0.0.1:8787 --token ivb_...
  ivarabase ops                         list every operation
  ivarabase <group> <action> [--param value ...]
        e.g. ivarabase projects create --slug shop
             ivarabase db query --project shop --sql "select now()"
             ivarabase functions deploy --project shop --name hello --code @hello.ts
             ivarabase backups create --project shop --verify
  ivarabase call <operation> ['{"json":"args"}']
  ivarabase mcp                         MCP stdio bridge (for clients without HTTP MCP support)

Values: @file reads a file, - reads stdin, JSON objects/arrays are accepted for object/array params.
`

func main() {
	log.SetFlags(log.LstdFlags)
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = cmdServe(os.Args[2:])
	case "init":
		err = cmdInit(os.Args[2:])
	case "token":
		err = cmdToken(os.Args[2:])
	case "reset-password":
		err = cmdResetPassword(os.Args[2:])
	case "dr":
		err = cmdDR(os.Args[2:])
	case "service":
		err = cmdService(os.Args[2:])
	case "config":
		err = cmdConfig(os.Args[2:])
	case "doctor":
		err = cmdDoctor(os.Args[2:])
	case "proxy-config":
		err = cmdProxyConfig(os.Args[2:])
	case "tls-sync":
		err = cmdTLSSync(os.Args[2:])
	case "plan":
		err = cmdPlan(os.Args[2:])
	case "apply":
		err = cmdApply(os.Args[2:])
	case "export":
		err = cmdExport(os.Args[2:])
	case "login":
		err = cmdLogin(os.Args[2:])
	case "mcp":
		err = cmdMCPBridge(os.Args[2:])
	case "ops":
		err = cmdOps()
	case "call":
		err = cmdCall(os.Args[2:])
	case "healthcheck":
		err = cmdHealthcheck(os.Args[2:])
	case "env":
		for _, e := range core.EnvSettings() {
			fmt.Printf("  %-34s %s\n", e[0], e[1])
		}
	case "version", "--version", "-v":
		fmt.Println("ivarabase", core.Version)
		fmt.Println(core.RepoURL)
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		err = cmdOp(os.Args[1:])
	}
	if err != nil {
		var ce *cliError
		if errors.As(err, &ce) {
			os.Stderr.Write(ce.body)
			if len(ce.body) == 0 || ce.body[len(ce.body)-1] != '\n' {
				os.Stderr.Write([]byte("\n"))
			}
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------- server commands

func printInit(res *core.InitResult, a *core.App) {
	if res == nil || (!res.Fresh && res.RecoveryKey == "" && res.OwnerPassword == "") {
		return
	}
	var b strings.Builder
	b.WriteString("\n================ IvaraBase is initialized ================\n")
	if res.OwnerPassword != "" {
		fmt.Fprintf(&b, " UI:        %s\n user:      %s\n password:  %s\n", a.Cfg.PublicURL, res.OwnerUser, res.OwnerPassword)
	}
	if res.RecoveryKey != "" {
		fmt.Fprintf(&b, "\n RECOVERY KEY (decrypts every backup - store it in a password manager NOW):\n   %s\n", res.RecoveryKey)
	}
	fmt.Fprintf(&b, "\n data dir:  %s\n==========================================================\n\n", a.Cfg.DataDir)
	creds := a.Cfg.Path("secrets", "INITIAL_CREDENTIALS.txt")
	os.WriteFile(creds, []byte(b.String()+"Delete this file once you have saved these values.\n"), 0o600)
	if isTerminal(os.Stdout) {
		fmt.Print(b.String())
		return
	}
	// Under systemd/launchd stdout is a log anyone in the adm group (or with
	// journal access) can read: never put the password or recovery key there.
	fmt.Printf("IvaraBase is initialized: panel %s. The login and RECOVERY KEY are in %s (owner-only) - save them, then delete the file.\n",
		a.Cfg.PublicURL, creds)
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func dataFlag(fs *flag.FlagSet) *string {
	return fs.String("data", core.DefaultDataDir(), "data directory")
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	data := dataFlag(fs)
	listen := fs.String("listen", "", "HTTP listen address (default 127.0.0.1:8787)")
	public := fs.String("public-url", "", "public base URL")
	fs.Parse(args)
	// first start only: IVARA_ADMIN_USER / IVARA_ADMIN_PASSWORD set the owner
	// login (for hosts where running a command in the container is awkward)
	user := os.Getenv("IVARA_ADMIN_USER")
	if user == "" {
		user = "admin"
	}
	pw := os.Getenv("IVARA_ADMIN_PASSWORD")
	if pw == "" {
		pw = os.Getenv("IVARA_OWNER_PASSWORD")
	}
	if pw != "" && len(pw) < 10 {
		return errors.New("IVARA_ADMIN_PASSWORD must be at least 10 characters")
	}
	a, res, err := core.Open(*data, core.InitOptions{Listen: *listen, PublicURL: *public, OwnerUser: user, OwnerPassword: pw})
	if err != nil {
		return err
	}
	if res != nil && pw != "" && res.OwnerPassword == pw {
		res.OwnerPassword = "(the IVARA_ADMIN_PASSWORD you set)"
	}
	a.PG.Owned = true // the server owns the cluster's lifecycle
	defer a.Close()
	printInit(res, a)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a.UpgradeProjects()
	a.Background(ctx)
	err = core.NewServer(a, web.Static()).ListenAndServe(ctx)
	log.Printf("shutting down")
	return err
}

// cmdHealthcheck is for Docker's HEALTHCHECK (the image has no curl): exit 0
// when the local server answers /healthz.
func cmdHealthcheck(args []string) error {
	fs := flag.NewFlagSet("healthcheck", flag.ExitOnError)
	data := dataFlag(fs)
	fs.Parse(args)
	addr := "127.0.0.1:8787"
	if cfg, err := core.LoadConfig(*data); err == nil && cfg.Listen != "" {
		addr = cfg.Listen
	}
	if host, port, err := net.SplitHostPort(addr); err == nil && (host == "" || host == "0.0.0.0" || host == "::") {
		addr = net.JoinHostPort("127.0.0.1", port)
	}
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("healthz returned %s", resp.Status)
	}
	return nil
}

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	data := dataFlag(fs)
	user := fs.String("user", "admin", "owner username")
	pw := fs.String("password", os.Getenv("IVARA_OWNER_PASSWORD"), "owner password (random when empty)")
	listen := fs.String("listen", "", "HTTP listen address to store in config.json")
	public := fs.String("public-url", "", "public base URL to store in config.json")
	fs.Parse(args)
	a, res, err := core.Open(*data, core.InitOptions{Persist: true, OwnerUser: *user, OwnerPassword: *pw, Listen: *listen, PublicURL: *public})
	if err != nil {
		return err
	}
	defer a.Close()
	if res.Fresh {
		printInit(res, a)
	} else {
		fmt.Println("already initialized:", a.Cfg.DataDir)
	}
	return nil
}

func cmdToken(args []string) error {
	if len(args) == 0 || args[0] != "create" {
		return errors.New("usage: ivarabase token create --name NAME [--level write] [--projects a,b] [--allow-destructive]")
	}
	fs := flag.NewFlagSet("token create", flag.ExitOnError)
	data := dataFlag(fs)
	name := fs.String("name", "", "token label")
	level := fs.String("level", "write", "read | write | admin")
	projects := fs.String("projects", "", "comma-separated project slugs (empty = all)")
	allowD := fs.Bool("allow-destructive", false, "skip owner approval for destructive calls")
	days := fs.Int("expires-days", 0, "0 = never")
	fs.Parse(args[1:])
	a, _, err := core.Open(*data, core.InitOptions{})
	if err != nil {
		return err
	}
	defer a.Close()
	var lv core.Level
	switch *level {
	case "read":
		lv = core.LevelRead
	case "write":
		lv = core.LevelWrite
	case "admin":
		lv = core.LevelAdmin
	default:
		return errors.New("--level must be read, write or admin")
	}
	var ps []string
	for _, p := range strings.Split(*projects, ",") {
		if p = strings.TrimSpace(p); p != "" {
			ps = append(ps, p)
		}
	}
	res, err := a.CreateToken(*name, lv, ps, *allowD, *days)
	if err != nil {
		return err
	}
	return printJSON(res)
}

func cmdResetPassword(args []string) error {
	fs := flag.NewFlagSet("reset-password", flag.ExitOnError)
	data := dataFlag(fs)
	user := fs.String("user", "admin", "owner username")
	pw := fs.String("password", "", "new password (random when empty)")
	fs.Parse(args)
	a, _, err := core.Open(*data, core.InitOptions{})
	if err != nil {
		return err
	}
	defer a.Close()
	newPw, err := a.ResetPassword(*user, *pw)
	if err != nil {
		return err
	}
	fmt.Printf("password for %s reset to: %s\n", *user, newPw)
	return nil
}

func cmdDR(args []string) error {
	if len(args) == 0 || args[0] != "restore" {
		return errors.New("usage: ivarabase dr restore --from DIR | --s3-... --recovery-key KEY")
	}
	fs := flag.NewFlagSet("dr restore", flag.ExitOnError)
	data := dataFlag(fs)
	from := fs.String("from", "", "local directory holding .ivbak bundles")
	key := fs.String("recovery-key", os.Getenv("IVARA_RECOVERY_KEY"), "recovery key of the old installation")
	only := fs.String("projects", "", "only these projects (comma-separated)")
	s3 := core.S3Config{}
	fs.StringVar(&s3.Endpoint, "s3-endpoint", os.Getenv("IVARA_S3_ENDPOINT"), "")
	fs.StringVar(&s3.Region, "s3-region", os.Getenv("IVARA_S3_REGION"), "")
	fs.StringVar(&s3.Bucket, "s3-bucket", os.Getenv("IVARA_S3_BUCKET"), "")
	fs.StringVar(&s3.Prefix, "s3-prefix", os.Getenv("IVARA_S3_PREFIX"), "")
	fs.StringVar(&s3.AccessKey, "s3-access-key", os.Getenv("IVARA_S3_ACCESS_KEY"), "")
	fs.StringVar(&s3.SecretKey, "s3-secret-key", os.Getenv("IVARA_S3_SECRET_KEY"), "")
	fs.Parse(args[1:])
	if *key == "" {
		return errors.New("--recovery-key is required (it is the key printed when the old server was initialized)")
	}
	if *from == "" && s3.Endpoint == "" {
		return errors.New("give --from DIR or the --s3-* flags")
	}
	a, res, err := core.Open(*data, core.InitOptions{RecoveryKey: *key})
	if err != nil {
		return err
	}
	defer a.Close()
	printInit(res, a)
	if *from != "" {
		n, err := copyBundles(*from, a.Cfg.Path("backups"))
		if err != nil {
			return err
		}
		fmt.Printf("copied %d bundle(s) from %s\n", n, *from)
	}
	if s3.Endpoint != "" {
		if err := a.SetBackupTarget(s3); err != nil {
			return err
		}
		fmt.Println("off-site target configured:", s3.Bucket)
	}
	if err := a.CheckRecoveryKey(context.Background()); err != nil {
		return err
	}
	var ps []string
	for _, p := range strings.Split(*only, ",") {
		if p = strings.TrimSpace(p); p != "" {
			ps = append(ps, p)
		}
	}
	results, err := a.RestoreAll(context.Background(), ps)
	if err != nil {
		return err
	}
	return printJSON(results)
}

func copyBundles(from, to string) (int, error) {
	absFrom, _ := filepath.Abs(from)
	absTo, _ := filepath.Abs(to)
	if absFrom == absTo {
		return 0, nil
	}
	entries, err := os.ReadDir(from)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".ivbak") {
			continue
		}
		src, err := os.Open(filepath.Join(from, e.Name()))
		if err != nil {
			return n, err
		}
		dst, err := os.OpenFile(filepath.Join(to, e.Name()), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			src.Close()
			return n, err
		}
		_, err = io.Copy(dst, src)
		src.Close()
		dst.Close()
		if err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// ---------------------------------------------------------------- client

type cliConfig struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

func cliConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "ivarabase", "cli.json")
}

func loadCLI() cliConfig {
	var c cliConfig
	if b, err := os.ReadFile(cliConfigPath()); err == nil {
		json.Unmarshal(b, &c)
	}
	if v := os.Getenv("IVARA_URL"); v != "" {
		c.URL = v
	}
	if v := os.Getenv("IVARA_TOKEN"); v != "" {
		c.Token = v
	}
	if c.URL == "" {
		c.URL = "http://127.0.0.1:8787"
	}
	c.URL = strings.TrimRight(c.URL, "/")
	return c
}

type cliError struct{ body []byte }

func (e *cliError) Error() string { return string(e.body) }

var httpClient = &http.Client{Timeout: 30 * time.Minute}

func apiCall(c cliConfig, method, path string, body any) ([]byte, int, error) {
	if c.Token == "" {
		return nil, 0, errors.New("no token: run `ivarabase login --url URL --token ivb_...` or set IVARA_TOKEN")
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.URL+path, rd)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("cannot reach %s: %w", c.URL, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	return out, resp.StatusCode, err
}

func cmdLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	url := fs.String("url", "http://127.0.0.1:8787", "server URL")
	token := fs.String("token", "", "API token (ivb_...)")
	fs.Parse(args)
	c := cliConfig{URL: strings.TrimRight(*url, "/"), Token: *token}
	out, status, err := apiCall(c, "GET", "/api/v1/me", nil)
	if err != nil {
		return err
	}
	if status != 200 {
		return &cliError{out}
	}
	os.MkdirAll(filepath.Dir(cliConfigPath()), 0o700)
	b, _ := json.MarshalIndent(c, "", "  ")
	if err := os.WriteFile(cliConfigPath(), b, 0o600); err != nil {
		return err
	}
	fmt.Printf("saved to %s\n%s", cliConfigPath(), out)
	return nil
}

func cmdOps() error {
	c := loadCLI()
	out, status, err := apiCall(c, "GET", "/api/v1/ops", nil)
	if err != nil {
		return err
	}
	if status != 200 {
		return &cliError{out}
	}
	var ops []struct {
		Name, Summary, Level string
		Destructive          bool
		Params               []struct {
			Name     string
			Type     string
			Required bool
		}
	}
	json.Unmarshal(out, &ops)
	sort.Slice(ops, func(i, j int) bool { return ops[i].Name < ops[j].Name })
	for _, o := range ops {
		var ps []string
		for _, p := range o.Params {
			s := "--" + p.Name
			if !p.Required {
				s = "[" + s + "]"
			}
			ps = append(ps, s)
		}
		flag := ""
		if o.Destructive {
			flag = " (destructive)"
		}
		fmt.Printf("%-28s %-6s %s%s\n%30s%s\n", strings.Replace(o.Name, ".", " ", 1), o.Level, o.Summary, flag, "", strings.Join(ps, " "))
	}
	return nil
}

func cmdCall(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: ivarabase call <operation> ['{json}']")
	}
	body := map[string]any{}
	if len(args) > 1 {
		raw := args[1]
		if raw == "-" {
			b, _ := io.ReadAll(os.Stdin)
			raw = string(b)
		}
		if err := json.Unmarshal([]byte(raw), &body); err != nil {
			return fmt.Errorf("arguments must be a JSON object: %w", err)
		}
	}
	return runOp(args[0], body)
}

// cmdOp maps `ivarabase projects create --slug x` to projects.create.
func cmdOp(args []string) error {
	if len(args) < 2 || strings.HasPrefix(args[1], "-") {
		return fmt.Errorf("unknown command %q (run `ivarabase help`)", args[0])
	}
	op := args[0] + "." + strings.ReplaceAll(args[1], "-", "_")
	body := map[string]any{}
	rest := args[2:]
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		if !strings.HasPrefix(a, "--") {
			return fmt.Errorf("unexpected argument %q (use --name value)", a)
		}
		key, val, hasEq := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		key = strings.ReplaceAll(key, "-", "_") // only the name; values keep their dashes
		if hasEq {
			// --name=value
		} else if i+1 < len(rest) && !isFlag(rest[i+1]) { // a value may start with dashes (-----BEGIN ...)
			val = rest[i+1]
			i++
		} else {
			body[key] = true
			continue
		}
		switch {
		case val == "-":
			b, _ := io.ReadAll(os.Stdin)
			val = string(b)
		case strings.HasPrefix(val, "@"):
			b, err := os.ReadFile(val[1:])
			if err != nil {
				return err
			}
			val = string(b)
		}
		body[key] = val
	}
	if v, ok := body["json"]; ok {
		delete(body, "json")
		if err := json.Unmarshal([]byte(fmt.Sprint(v)), &body); err != nil {
			return fmt.Errorf("--json: %w", err)
		}
	}
	if op == "db.types" { // print the TypeScript itself, or write it to --out
		out, _ := body["out"].(string)
		delete(body, "out")
		var res map[string]any
		if err := apiJSON("POST", "/api/v1/ops/db.types", body, &res); err != nil {
			return err
		}
		ts, _ := res["types"].(string)
		if out == "" {
			fmt.Print(ts)
			return nil
		}
		if err := os.WriteFile(out, []byte(ts), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote %s\n", out)
		return nil
	}
	if op == "functions.deploy" {
		if dir, ok := body["dir"].(string); ok {
			delete(body, "dir")
			files, err := readFunctionDir(dir)
			if err != nil {
				return err
			}
			body["files"] = files
			if _, ok := body["name"]; !ok {
				body["name"] = strings.ToLower(filepath.Base(filepath.Clean(dir)))
			}
			fmt.Fprintf(os.Stderr, "deploying %d file(s) from %s\n", len(files), dir)
		}
	}
	return runOp(op, body)
}

// readFunctionDir packs a function folder (Supabase layout works too:
// supabase/functions/<name>) into path -> source.
func readFunctionDir(dir string) (map[string]string, error) {
	files := map[string]string{}
	exts := map[string]bool{".ts": true, ".tsx": true, ".js": true, ".mjs": true, ".cjs": true, ".jsx": true, ".json": true, ".jsonc": true}
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		name := info.Name()
		if info.IsDir() {
			if p != dir && (strings.HasPrefix(name, ".") || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !exts[strings.ToLower(filepath.Ext(name))] || info.Size() > 2<<20 {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = string(b)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no .ts/.js/.json files found in %s", dir)
	}
	return files, nil
}

func runOp(op string, body map[string]any) error {
	c := loadCLI()
	out, status, err := apiCall(c, "POST", "/api/v1/ops/"+op, body)
	if err != nil {
		return err
	}
	if status >= 400 {
		return &cliError{out}
	}
	os.Stdout.Write(prettyJSON(out))
	return nil
}

func prettyJSON(b []byte) []byte {
	var buf bytes.Buffer
	if json.Indent(&buf, b, "", "  ") != nil {
		return b
	}
	out := buf.Bytes()
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	return out
}

func printJSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

// cmdMCPBridge relays newline-delimited JSON-RPC on stdio to the HTTP /mcp endpoint.
func cmdMCPBridge(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	url := fs.String("url", "", "server URL (default from login / IVARA_URL)")
	token := fs.String("token", "", "API token (default from login / IVARA_TOKEN)")
	fs.Parse(args)
	c := loadCLI()
	if *url != "" {
		c.URL = strings.TrimRight(*url, "/")
	}
	if *token != "" {
		c.Token = *token
	}
	if c.Token == "" {
		return errors.New("no token: pass --token or set IVARA_TOKEN")
	}
	in := bufio.NewReaderSize(os.Stdin, 1<<20)
	out := bufio.NewWriter(os.Stdout)
	for {
		line, err := in.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			req, _ := http.NewRequest("POST", c.URL+"/mcp", bytes.NewReader(line))
			req.Header.Set("Authorization", "Bearer "+c.Token)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			resp, herr := httpClient.Do(req)
			if herr != nil {
				var probe struct {
					ID json.RawMessage `json:"id"`
				}
				json.Unmarshal(line, &probe)
				if len(probe.ID) > 0 {
					msg, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": probe.ID, "error": map[string]any{"code": -32000, "message": "ivarabase unreachable: " + herr.Error()}})
					out.Write(append(msg, '\n'))
					out.Flush()
				}
			} else {
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 202 && len(bytes.TrimSpace(body)) > 0 {
					var compact bytes.Buffer
					if json.Compact(&compact, body) == nil {
						body = compact.Bytes()
					}
					out.Write(append(body, '\n'))
					out.Flush()
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// isFlag: --name, not a value that happens to start with dashes.
func isFlag(s string) bool {
	return len(s) > 2 && s[0] == '-' && s[1] == '-' && (s[2] >= 'a' && s[2] <= 'z' || s[2] >= 'A' && s[2] <= 'Z')
}
