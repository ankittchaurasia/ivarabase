package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// ivarabase.yaml - a project as files:
//
//	project: shop
//	name: Shop
//	migrations: migrations          # *.sql, applied in file-name order
//	functions_dir: functions        # one folder per function
//	functions:
//	  stripe-webhook: { verify_jwt: false }
//	  nightly: { schedule: "0 3 * * *" }
//	secrets:
//	  STRIPE_KEY: ${STRIPE_KEY}     # from the environment or a .env file next to ivarabase.yaml
//	auth:
//	  site_url: https://myapp.com
//	  redirect_urls: [http://localhost:3000/*]
//	  confirm_email: true
//	  google: { enabled: true, client_id: abc, client_secret: ${GOOGLE_CLIENT_SECRET} }
//	  apple: { enabled: true, client_id: com.myapp.web, team_id: ABCDE12345, key_id: XYZ9876543,
//	           private_key: ${APPLE_PRIVATE_KEY}, bundle_ids: [com.myapp.ios] }
//	storage:
//	  buckets:
//	    avatars: { public: true, file_size_limit: 1MB, allowed_mime_types: [image/*] }

type yamlFunction struct {
	VerifyJWT  *bool  `yaml:"verify_jwt,omitempty" json:"verify_jwt,omitempty"`
	Schedule   string `yaml:"schedule,omitempty" json:"schedule,omitempty"`
	Entrypoint string `yaml:"entrypoint,omitempty" json:"entrypoint,omitempty"`
	TimeoutS   int    `yaml:"timeout_s,omitempty" json:"timeout_s,omitempty"`
	MemoryMB   int    `yaml:"memory_mb,omitempty" json:"memory_mb,omitempty"`
}

type yamlBucket struct {
	Public           bool     `yaml:"public" json:"public"`
	FileSizeLimit    string   `yaml:"file_size_limit,omitempty" json:"file_size_limit,omitempty"`
	AllowedMimeTypes []string `yaml:"allowed_mime_types,omitempty" json:"allowed_mime_types,omitempty"`
}

type yamlSpec struct {
	Project      string                  `yaml:"project"`
	Name         string                  `yaml:"name,omitempty"`
	Migrations   string                  `yaml:"migrations,omitempty"`
	FunctionsDir string                  `yaml:"functions_dir,omitempty"`
	Functions    map[string]yamlFunction `yaml:"functions,omitempty"`
	Secrets      map[string]string       `yaml:"secrets,omitempty"`
	Auth         map[string]any          `yaml:"auth,omitempty"`
	Storage      *struct {
		Buckets map[string]yamlBucket `yaml:"buckets"`
	} `yaml:"storage,omitempty"`
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// loadDotEnv reads KEY=VALUE lines without overriding the real environment.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(l, "export "), "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`)
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, v)
		}
	}
}

// buildSpec reads ivarabase.yaml plus the migrations and functions folders.
func buildSpec(file string) (map[string]any, string, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, "", err
	}
	base := filepath.Dir(file)
	loadDotEnv(filepath.Join(base, ".env"))
	// ${NAME}: simple values (numbers, ids, booleans) are substituted into the
	// text so typed fields still work; anything else - multi-line keys, values
	// with spaces, '#', quotes or ':' - goes in through a placeholder after the
	// YAML is parsed, so a secret can never be read as YAML.
	var missing []string
	var deferred []string
	lines := strings.Split(string(raw), "\n")
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue // comments may mention ${NAME} without needing it
		}
		lines[i] = envRef.ReplaceAllStringFunc(l, func(m string) string {
			name := envRef.FindStringSubmatch(m)[1]
			v, ok := os.LookupEnv(name)
			if !ok {
				missing = append(missing, name)
			}
			if plainValue.MatchString(v) {
				return v
			}
			deferred = append(deferred, v)
			return fmt.Sprintf("__IVB_ENV_%d__", len(deferred)-1)
		})
	}
	expanded := strings.Join(lines, "\n")
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, "", fmt.Errorf("these variables are referenced in %s but not set: %s (export them or put them in %s)",
			file, strings.Join(missing, ", "), filepath.Join(base, ".env"))
	}
	var y yamlSpec
	dec := yaml.NewDecoder(strings.NewReader(expanded))
	dec.KnownFields(true)
	if err := dec.Decode(&y); err != nil {
		return nil, "", fmt.Errorf("%s: %w", file, err)
	}
	if y.Project == "" {
		return nil, "", fmt.Errorf("%s: project is required", file)
	}
	spec := map[string]any{"project": y.Project}
	if y.Name != "" {
		spec["name"] = y.Name
	}

	migDir := y.Migrations
	if migDir == "" {
		migDir = "migrations"
	}
	if entries, err := os.ReadDir(filepath.Join(base, migDir)); err == nil {
		var migs []map[string]any
		var names []string
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for _, n := range names {
			b, err := os.ReadFile(filepath.Join(base, migDir, n))
			if err != nil {
				return nil, "", err
			}
			migs = append(migs, map[string]any{"name": strings.TrimSuffix(n, ".sql"), "sql": string(b)})
		}
		spec["migrations"] = migs
	} else if y.Migrations != "" {
		return nil, "", fmt.Errorf("migrations folder %s: %w", migDir, err)
	}

	fnDir := y.FunctionsDir
	if fnDir == "" {
		fnDir = "functions"
	}
	fns := map[string]any{}
	if entries, err := os.ReadDir(filepath.Join(base, fnDir)); err == nil {
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || strings.HasPrefix(e.Name(), "_") {
				continue
			}
			files, err := readFunctionDir(filepath.Join(base, fnDir, e.Name()))
			if err != nil {
				return nil, "", err
			}
			f := map[string]any{"files": files}
			if s, ok := y.Functions[e.Name()]; ok {
				b, _ := json.Marshal(s)
				json.Unmarshal(b, &f)
				f["files"] = files
			}
			fns[e.Name()] = f
		}
	}
	for n := range y.Functions {
		if _, ok := fns[n]; !ok {
			return nil, "", fmt.Errorf("functions.%s has settings but there is no folder %s", n, filepath.Join(fnDir, n))
		}
	}
	if len(fns) > 0 {
		spec["functions"] = fns
	}
	if y.Secrets != nil {
		spec["secrets"] = y.Secrets
	}
	if y.Auth != nil {
		spec["auth"] = y.Auth
	}
	if y.Storage != nil {
		spec["storage"] = y.Storage
	}
	if len(deferred) > 0 {
		b, _ := json.Marshal(spec) // typed parts (buckets, function settings) become plain maps
		var m map[string]any
		json.Unmarshal(b, &m)
		spec = resolvePlaceholders(m, deferred).(map[string]any)
		y.Project, _ = spec["project"].(string)
	}
	return spec, y.Project, nil
}

var (
	plainValue  = regexp.MustCompile(`^[A-Za-z0-9._/@+=-]*$`)
	placeholder = regexp.MustCompile(`__IVB_ENV_(\d+)__`)
)

func resolvePlaceholders(v any, vals []string) any {
	switch x := v.(type) {
	case string:
		return placeholder.ReplaceAllStringFunc(x, func(m string) string {
			var i int
			fmt.Sscanf(placeholder.FindStringSubmatch(m)[1], "%d", &i)
			return vals[i]
		})
	case map[string]any:
		for k, e := range x {
			x[k] = resolvePlaceholders(e, vals)
		}
	case []any:
		for i, e := range x {
			x[i] = resolvePlaceholders(e, vals)
		}
	}
	return v
}

func printPlan(changes []any) {
	sym := map[string]string{"create": "+", "apply": "+", "update": "~", "delete": "-", "error": "!", "note": " "}
	for _, c := range changes {
		m, _ := c.(map[string]any)
		action, _ := m["action"].(string)
		line := fmt.Sprintf("  %s %-10s %-7s %s", sym[action], m["area"], action, m["target"])
		if d, _ := m["detail"].(string); d != "" {
			line += "  (" + d + ")"
		}
		if e, ok := m["error"]; ok && e != nil {
			b, _ := json.Marshal(e)
			line += "\n      error: " + string(b)
		}
		if r, ok := m["result"].(map[string]any); ok {
			if id, ok := r["approval_required"]; ok {
				line += fmt.Sprintf("\n      waiting for owner approval: %v", id)
			}
		}
		fmt.Println(line)
	}
}

func configFlags(name string, args []string) (*flag.FlagSet, *string, *bool, *bool) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	file := fs.String("f", "ivarabase.yaml", "config file")
	prune := fs.Bool("prune", false, "also delete functions, secrets and buckets that are not in the files")
	yes := fs.Bool("yes", false, "apply without asking")
	fs.Parse(args)
	return fs, file, prune, yes
}

func cmdPlan(args []string) error {
	_, file, prune, _ := configFlags("plan", args)
	spec, project, err := buildSpec(*file)
	if err != nil {
		return err
	}
	var out map[string]any
	if err := apiJSON("POST", "/api/v1/ops/config.plan", map[string]any{"spec": spec, "prune": *prune}, &out); err != nil {
		return err
	}
	changes, _ := out["changes"].([]any)
	if len(changes) == 0 {
		fmt.Printf("%s is up to date with %s\n", project, *file)
		return nil
	}
	fmt.Printf("Plan for %s:\n", project)
	printPlan(changes)
	next := "Run `ivarabase apply` to make these changes."
	if m, _ := out["summary"].(map[string]any); m["error"] != nil {
		next = "Fix the errors above first - apply refuses to run a plan with errors."
	}
	fmt.Printf("\n%s\n%s\n", summaryLine(out["summary"]), next)
	return nil
}

func cmdApply(args []string) error {
	_, file, prune, yes := configFlags("apply", args)
	spec, project, err := buildSpec(*file)
	if err != nil {
		return err
	}
	var plan map[string]any
	if err := apiJSON("POST", "/api/v1/ops/config.plan", map[string]any{"spec": spec, "prune": *prune}, &plan); err != nil {
		return err
	}
	changes, _ := plan["changes"].([]any)
	if len(changes) == 0 {
		fmt.Printf("%s is up to date - nothing to apply\n", project)
		return nil
	}
	fmt.Printf("Changes for %s:\n", project)
	printPlan(changes)
	if !*yes {
		fmt.Print("\nApply these changes? [y/N] ")
		var answer string
		fmt.Scanln(&answer)
		if !strings.EqualFold(strings.TrimSpace(answer), "y") && !strings.EqualFold(strings.TrimSpace(answer), "yes") {
			return errors.New("cancelled")
		}
	}
	body := map[string]any{"spec": spec, "prune": *prune}
	if *prune {
		body["confirm"] = true
	}
	var out map[string]any
	if err := apiJSON("POST", "/api/v1/ops/config.apply", body, &out); err != nil {
		return err
	}
	if out["status"] == "approval_required" {
		fmt.Printf("\nDeleting things needs the owner's approval: %v (approve it in the UI, then run apply again)\n", out["approval_id"])
		return nil
	}
	fmt.Println("\nResult:")
	res, _ := out["changes"].([]any)
	printPlan(res)
	if s, ok := out["stopped"].(string); ok {
		return errors.New(s)
	}
	fmt.Printf("\napplied %v change(s)\n", out["applied"])
	return nil
}

func cmdExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	project := fs.String("project", "", "project slug")
	dir := fs.String("dir", ".", "where to write ivarabase.yaml, migrations/ and functions/")
	force := fs.Bool("force", false, "overwrite an existing ivarabase.yaml")
	fs.Parse(args)
	if *project == "" {
		return errors.New("usage: ivarabase export --project SLUG [--dir .]")
	}
	yamlPath := filepath.Join(*dir, "ivarabase.yaml")
	if _, err := os.Stat(yamlPath); err == nil && !*force {
		return fmt.Errorf("%s exists (use --force to overwrite)", yamlPath)
	}
	var spec map[string]any
	if err := apiJSON("POST", "/api/v1/ops/config.export", map[string]any{"project": *project}, &spec); err != nil {
		return err
	}
	y := yamlSpec{Project: *project, Migrations: "migrations", FunctionsDir: "functions", Functions: map[string]yamlFunction{}}
	if n, _ := spec["name"].(string); n != "" && n != *project {
		y.Name = n
	}
	os.MkdirAll(filepath.Join(*dir, "migrations"), 0o755)
	migs, _ := spec["migrations"].([]any)
	for _, m := range migs {
		mm, _ := m.(map[string]any)
		fname := fmt.Sprintf("%s_%s.sql", mm["version"], mm["name"])
		if err := os.WriteFile(filepath.Join(*dir, "migrations", fname), []byte(fmt.Sprint(mm["sql"])), 0o644); err != nil {
			return err
		}
	}
	fns, _ := spec["functions"].(map[string]any)
	for name, v := range fns {
		f, _ := v.(map[string]any)
		files, _ := f["files"].(map[string]any)
		for p, src := range files {
			full := filepath.Join(*dir, "functions", name, filepath.FromSlash(p))
			os.MkdirAll(filepath.Dir(full), 0o755)
			if err := os.WriteFile(full, []byte(fmt.Sprint(src)), 0o644); err != nil {
				return err
			}
		}
		yf := yamlFunction{}
		if vj, ok := f["verify_jwt"].(bool); ok && !vj {
			yf.VerifyJWT = &vj
		}
		if s, _ := f["schedule"].(string); s != "" {
			yf.Schedule = s
		}
		if e, _ := f["entrypoint"].(string); e != "" && e != "index.ts" {
			yf.Entrypoint = e
		}
		if t, _ := f["timeout_s"].(float64); t != 0 && t != 30 {
			yf.TimeoutS = int(t)
		}
		if m, _ := f["memory_mb"].(float64); m != 0 && m != 128 {
			yf.MemoryMB = int(m)
		}
		if yf != (yamlFunction{}) {
			y.Functions[name] = yf
		}
	}
	if s, _ := spec["secrets"].(map[string]any); len(s) > 0 {
		y.Secrets = map[string]string{}
		for k, v := range s {
			y.Secrets[k] = fmt.Sprint(v)
		}
	}
	if a, _ := spec["auth"].(map[string]any); a != nil {
		y.Auth = pruneEmpty(a).(map[string]any)
	}
	if st, _ := spec["storage"].(map[string]any); st != nil {
		if b, _ := st["buckets"].(map[string]any); len(b) > 0 {
			raw, _ := json.Marshal(st)
			y.Storage = &struct {
				Buckets map[string]yamlBucket `yaml:"buckets"`
			}{}
			json.Unmarshal(raw, y.Storage)
		}
	}
	out, err := yaml.Marshal(y)
	if err != nil {
		return err
	}
	header := "# IvaraBase project config - `ivarabase plan` shows differences, `ivarabase apply` applies them.\n# ${NAME} values come from the environment or a .env file next to this file.\n"
	if err := os.WriteFile(yamlPath, append([]byte(header), out...), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s, %d migration(s), %d function(s)\n", yamlPath, len(migs), len(fns))
	if len(y.Secrets) > 0 {
		fmt.Println("secrets are written as ${NAME} placeholders - put the values in a .env file (and keep it out of git)")
	}
	return nil
}

// pruneEmpty drops empty strings / zero ports so the exported yaml stays short.
func pruneEmpty(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, x := range t {
			x = pruneEmpty(x)
			switch y := x.(type) {
			case string:
				if y == "" {
					continue
				}
			case map[string]any:
				if len(y) == 0 {
					continue
				}
			case []any:
				if len(y) == 0 {
					continue
				}
			case bool:
				if !y && k == "enabled" {
					continue
				}
			}
			out[k] = x
		}
		return out
	}
	return v
}

// apiJSON calls the server with the CLI's credentials and decodes the reply.
func apiJSON(method, path string, body any, dst any) error {
	c := loadCLI()
	out, status, err := apiCall(c, method, path, body)
	if err != nil {
		return err
	}
	if status >= 400 {
		return &cliError{out}
	}
	return json.Unmarshal(out, dst)
}

func summaryLine(v any) string {
	m, _ := v.(map[string]any)
	var parts []string
	for _, k := range []string{"create", "apply", "update", "delete", "error"} {
		if n, ok := m[k].(float64); ok && n > 0 {
			label := map[string]string{"create": "to create", "apply": "to apply", "update": "to update", "delete": "to delete", "error": "error(s)"}[k]
			parts = append(parts, fmt.Sprintf("%d %s", int(n), label))
		}
	}
	return strings.Join(parts, ", ")
}
