package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"text/template"
	"time"

	"ivarabase/internal/core"
)

// `ivarabase service ...` installs IvaraBase as a background service that
// starts at boot and restarts on failure: systemd on Linux, launchd on macOS.

const (
	unitName    = "ivarabase"
	unitPath    = "/etc/systemd/system/ivarabase.service"
	launchLabel = "com.ivarabase.server"
)

var systemdUnit = template.Must(template.New("unit").Parse(`[Unit]
Description=IvaraBase (Postgres platform)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User={{.User}}
Group={{.Group}}
Environment=HOME={{.Home}}
Environment=PATH=/usr/local/bin:/usr/bin:/bin:{{.Home}}/.deno/bin
ExecStart={{.Bin}} serve --data {{.Data}}
Restart=always
RestartSec=3
# the server stops Postgres itself; give it time, then clean up the rest
KillMode=mixed
TimeoutStopSec=60
LimitNOFILE=65536
# bind :80 and :443 without running as root
AmbientCapabilities=CAP_NET_BIND_SERVICE
NoNewPrivileges=true

[Install]
WantedBy=multi-user.target
`))

var launchdPlist = template.Must(template.New("plist").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>{{.Label}}</string>
  <key>ProgramArguments</key>
  <array><string>{{.Bin}}</string><string>serve</string><string>--data</string><string>{{.Data}}</string></array>
  {{if .User}}<key>UserName</key><string>{{.User}}</string>{{end}}
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key><string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:{{.Home}}/.deno/bin</string>
    <key>HOME</key><string>{{.Home}}</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>5</integer>
  <key>ExitTimeOut</key><integer>60</integer>
  <key>StandardOutPath</key><string>{{.Log}}</string>
  <key>StandardErrorPath</key><string>{{.Log}}</string>
</dict>
</plist>
`))

func cmdService(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: ivarabase service install|uninstall|restart|status|logs")
	}
	if core.InContainer() {
		return errors.New("`ivarabase service` doesn't apply inside a container: Docker (restart: unless-stopped) already runs and restarts IvaraBase. " +
			"Run other commands with `docker exec -it ivarabase ivarabase <command>`")
	}
	switch args[0] {
	case "install":
		return serviceInstall(args[1:])
	case "uninstall":
		return serviceUninstall()
	case "restart":
		return serviceRestart()
	case "status":
		return serviceStatus()
	case "logs":
		return serviceLogs(args[1:])
	}
	return fmt.Errorf("unknown service command %q", args[0])
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".new"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	out.Close()
	return os.Rename(tmp, dst)
}

func serviceInstall(args []string) error {
	fs := flag.NewFlagSet("service install", flag.ExitOnError)
	data := fs.String("data", "", "data directory (Linux default /var/lib/ivarabase, macOS default ~/.ivarabase)")
	userName := fs.String("user", "", "account to run as (Linux default: a new 'ivarabase' system user)")
	domain := fs.String("domain", "", "serve https://<domain> and https://<slug>.<domain> with automatic certificates")
	email := fs.String("email", "", "Let's Encrypt contact email")
	proxy := fs.Bool("proxy", false, "HTTPS is handled by nginx/Caddy/Traefik in front: listen on 127.0.0.1:8787 only, never bind 80/443")
	certFile := fs.String("cert-file", "", "certificate PEM (with --proxy: the proxy's wildcard certificate, used for Postgres TLS)")
	keyFile := fs.String("key-file", "", "private key PEM for --cert-file")
	noCopy := fs.Bool("no-copy", false, "Linux: run the binary from where it is instead of /usr/local/bin/ivarabase")
	printOnly := fs.Bool("print", false, "only print the systemd unit / launchd plist that would be installed")
	fs.Parse(args)
	self, err := os.Executable()
	if err != nil {
		return err
	}
	self, _ = filepath.EvalSymlinks(self)

	if *printOnly {
		return printService(self, *data, *userName)
	}
	site := siteOpts{Domain: *domain, Email: *email, Proxy: *proxy, CertFile: *certFile, KeyFile: *keyFile}
	switch runtime.GOOS {
	case "linux":
		return installSystemd(self, *data, *userName, site, *noCopy)
	case "darwin":
		return installLaunchd(self, *data, *userName, site)
	}
	return fmt.Errorf("service install supports Linux (systemd) and macOS (launchd), not %s", runtime.GOOS)
}

type siteOpts struct {
	Domain, Email     string
	Proxy             bool
	CertFile, KeyFile string
}

func applyDomain(data string, o siteOpts) error {
	set := func(k, v string) error {
		if v == "" {
			return nil
		}
		_, err := core.SetConfig(data, k, v)
		return err
	}
	mode := ""
	if o.Proxy {
		mode = "proxy"
	}
	for _, kv := range [][2]string{{"domain", o.Domain}, {"tls.mode", mode}, {"tls.email", o.Email},
		{"tls.cert_file", o.CertFile}, {"tls.key_file", o.KeyFile}} {
		if err := set(kv[0], kv[1]); err != nil {
			return err
		}
	}
	return nil
}

func installSystemd(self, data, userName string, site siteOpts, noCopy bool) error {
	if os.Geteuid() != 0 {
		return errors.New("run with sudo: sudo ivarabase service install [--domain base.example.com --email you@example.com] (behind a reverse proxy: --domain base.example.com --proxy)")
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return errors.New("systemctl not found - this needs a systemd-based Linux")
	}
	if userName == "" {
		userName = "ivarabase"
	}
	if data == "" {
		data = "/var/lib/ivarabase"
	}
	u, err := user.Lookup(userName)
	if err != nil {
		if userName != "ivarabase" {
			return fmt.Errorf("user %q does not exist", userName)
		}
		fmt.Println("==> creating system user ivarabase")
		if err := run("useradd", "--system", "--home-dir", data, "--create-home", "--shell", "/usr/sbin/nologin", "ivarabase"); err != nil {
			return fmt.Errorf("useradd: %w", err)
		}
		if u, err = user.Lookup(userName); err != nil {
			return err
		}
	}
	g, _ := user.LookupGroupId(u.Gid)
	group := u.Gid
	if g != nil {
		group = g.Name
	}
	bin := self
	if !noCopy && self != "/usr/local/bin/ivarabase" {
		fmt.Println("==> installing binary to /usr/local/bin/ivarabase")
		if err := copyFile(self, "/usr/local/bin/ivarabase", 0o755); err != nil {
			return err
		}
		bin = "/usr/local/bin/ivarabase"
	}
	if err := os.MkdirAll(data, 0o700); err != nil {
		return err
	}
	if err := applyDomain(data, site); err != nil {
		return err
	}
	// before the chown: it may rewrite config.json and adds <data>/tls
	if err := setupCertSync(data, u.Username, bin); err != nil {
		return fmt.Errorf("certificate copy: %w", err)
	}
	if err := run("chown", "-R", u.Username+":"+group, data); err != nil {
		return err
	}
	home := u.HomeDir
	if home == "" {
		home = data
	}
	var buf strings.Builder
	systemdUnit.Execute(&buf, map[string]string{"User": u.Username, "Group": group, "Home": home, "Bin": bin, "Data": data})
	if err := os.WriteFile(unitPath, []byte(buf.String()), 0o644); err != nil {
		return err
	}
	fmt.Println("==> wrote", unitPath)
	run("systemctl", "daemon-reload")
	if err := run("systemctl", "enable", "--now", unitName); err != nil {
		return err
	}
	cfg, _ := core.LoadConfig(data)
	waitHealthy(cfg)
	fmt.Printf("\nIvaraBase is running as a service (user %s, data %s).\n", u.Username, data)
	if cfg != nil {
		fmt.Println("  panel:   ", cfg.PublicURL)
		if cfg.TLSMode() == "proxy" {
			fmt.Println("  proxy:    point your reverse proxy at http://127.0.0.1:8787 - ivarabase proxy-config --data", data, "prints the config")
		}
	}
	fmt.Println("  login:    sudo cat", filepath.Join(data, "secrets", "INITIAL_CREDENTIALS.txt"), "(first install only)")
	fmt.Println("  logs:     ivarabase service logs")
	fmt.Println("  status:   ivarabase service status")
	fmt.Println("  upgrade:  copy the new binary over /usr/local/bin/ivarabase, then sudo ivarabase service restart")
	return nil
}

func installLaunchd(self, data, userName string, site siteOpts) error {
	daemon := os.Geteuid() == 0
	var u *user.User
	var err error
	switch {
	case userName != "":
		u, err = user.Lookup(userName)
	case daemon && os.Getenv("SUDO_USER") != "":
		u, err = user.Lookup(os.Getenv("SUDO_USER"))
	default:
		u, err = user.Current()
	}
	if err != nil {
		return err
	}
	if u.Uid == "0" {
		return errors.New("PostgreSQL cannot run as root: pass --user <your account>")
	}
	if data == "" {
		data = filepath.Join(u.HomeDir, ".ivarabase")
	}
	if err := os.MkdirAll(filepath.Join(data, "logs"), 0o700); err != nil {
		return err
	}
	if err := applyDomain(data, site); err != nil {
		return err
	}
	if daemon {
		run("chown", "-R", u.Username, data)
	}
	path := filepath.Join(u.HomeDir, "Library", "LaunchAgents", launchLabel+".plist")
	target := "gui/" + u.Uid
	vars := map[string]string{"Label": launchLabel, "Bin": self, "Data": data, "Home": u.HomeDir, "Log": filepath.Join(data, "logs", "server.log")}
	if daemon { // system daemon: starts at boot without a login, can bind :80/:443
		path = "/Library/LaunchDaemons/" + launchLabel + ".plist"
		target = "system"
		vars["User"] = u.Username
	}
	var buf strings.Builder
	launchdPlist.Execute(&buf, vars)
	os.MkdirAll(filepath.Dir(path), 0o755)
	exec.Command("launchctl", "bootout", target+"/"+launchLabel).Run() // replace an old one
	if err := os.WriteFile(path, []byte(buf.String()), 0o644); err != nil {
		return err
	}
	fmt.Println("==> wrote", path)
	if err := run("launchctl", "bootstrap", target, path); err != nil {
		return fmt.Errorf("launchctl bootstrap: %w", err)
	}
	cfg, _ := core.LoadConfig(data)
	waitHealthy(cfg)
	fmt.Printf("\nIvaraBase is running as a launchd %s (data %s).\n", map[bool]string{true: "daemon", false: "agent - starts when you log in"}[daemon], data)
	if cfg != nil {
		fmt.Println("  panel:  ", cfg.PublicURL)
	}
	fmt.Println("  logs:    ivarabase service logs")
	fmt.Println("  stop:    ivarabase service uninstall (keeps your data)")
	return nil
}

func waitHealthy(cfg *core.Config) {
	if cfg == nil {
		return
	}
	fmt.Print("==> waiting for the server")
	for i := 0; i < 60; i++ {
		if resp, err := (&http.Client{Timeout: time.Second}).Get("http://" + cfg.Listen + "/healthz"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				fmt.Println(" - healthy")
				return
			}
		}
		fmt.Print(".")
		time.Sleep(time.Second)
	}
	fmt.Println(" - not healthy yet; check `ivarabase service logs`")
}

func launchTarget() (string, string) {
	if fileExists("/Library/LaunchDaemons/" + launchLabel + ".plist") {
		return "system", "/Library/LaunchDaemons/" + launchLabel + ".plist"
	}
	u, _ := user.Current()
	return "gui/" + u.Uid, filepath.Join(u.HomeDir, "Library", "LaunchAgents", launchLabel+".plist")
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func serviceUninstall() error {
	switch runtime.GOOS {
	case "linux":
		if os.Geteuid() != 0 {
			return errors.New("run with sudo")
		}
		run("systemctl", "disable", "--now", unitName)
		os.Remove(unitPath)
		removeCertSync()
		run("systemctl", "daemon-reload")
	case "darwin":
		target, path := launchTarget()
		exec.Command("launchctl", "bootout", target+"/"+launchLabel).Run()
		os.Remove(path)
	default:
		return fmt.Errorf("unsupported OS %s", runtime.GOOS)
	}
	fmt.Println("service removed; your data directory was not touched")
	return nil
}

func serviceRestart() error {
	switch runtime.GOOS {
	case "linux":
		return run("systemctl", "restart", unitName)
	case "darwin":
		target, _ := launchTarget()
		return run("launchctl", "kickstart", "-k", target+"/"+launchLabel)
	}
	return fmt.Errorf("unsupported OS %s", runtime.GOOS)
}

func serviceStatus() error {
	switch runtime.GOOS {
	case "linux":
		return run("systemctl", "status", unitName, "--no-pager", "--lines", "15")
	case "darwin":
		target, path := launchTarget()
		if !fileExists(path) {
			fmt.Println("not installed")
			return nil
		}
		out, err := exec.Command("launchctl", "print", target+"/"+launchLabel).CombinedOutput()
		if err != nil {
			fmt.Println("installed but not loaded:", strings.TrimSpace(string(out)))
			return nil
		}
		for _, line := range strings.Split(string(out), "\n") {
			l := strings.TrimSpace(line)
			if strings.HasPrefix(l, "state =") || strings.HasPrefix(l, "pid =") || strings.HasPrefix(l, "last exit code") || strings.HasPrefix(l, "runs =") {
				fmt.Println(l)
			}
		}
		fmt.Println("plist =", path)
		return nil
	}
	return fmt.Errorf("unsupported OS %s", runtime.GOOS)
}

func serviceLogs(args []string) error {
	fs := flag.NewFlagSet("service logs", flag.ExitOnError)
	data := fs.String("data", core.DefaultDataDir(), "data directory (macOS)")
	follow := fs.Bool("f", true, "follow")
	fs.Parse(args)
	switch runtime.GOOS {
	case "linux":
		a := []string{"-u", unitName, "-n", "100", "--no-pager"}
		if *follow {
			a = append(a, "-f")
		}
		return run("journalctl", a...)
	case "darwin":
		a := []string{"-n", "100"}
		if *follow {
			a = append(a, "-f")
		}
		return run("tail", append(a, filepath.Join(*data, "logs", "server.log"))...)
	}
	return fmt.Errorf("unsupported OS %s", runtime.GOOS)
}

// ---------------------------------------------------------------- config + doctor

func cmdConfig(args []string) error {
	fs := flag.NewFlagSet("config", flag.ExitOnError)
	data := fs.String("data", core.DefaultDataDir(), "data directory")
	if len(args) == 0 {
		args = []string{"show"}
	}
	sub := args[0]
	// allow flags before or after the positional KEY VALUE
	var flags, pos []string
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		if strings.HasPrefix(rest[i], "-") {
			flags = append(flags, rest[i])
			if !strings.Contains(rest[i], "=") && i+1 < len(rest) {
				flags = append(flags, rest[i+1])
				i++
			}
			continue
		}
		pos = append(pos, rest[i])
	}
	fs.Parse(flags)
	switch sub {
	case "show":
		cfg, err := core.LoadConfig(*data)
		if err != nil {
			return err
		}
		b, _ := json.MarshalIndent(cfg, "", "  ")
		fmt.Println(string(b))
		fmt.Printf("\neffective: panel %s · TLS %s\n", cfg.PublicURL, cfg.TLSMode())
		return nil
	case "set":
		rest := pos
		if len(rest) != 2 {
			var keys []string
			for k, d := range core.ConfigKeys {
				keys = append(keys, fmt.Sprintf("  %-27s %s", k, d))
			}
			sort.Strings(keys)
			return fmt.Errorf("usage: ivarabase config set KEY VALUE [--data DIR]\n\nkeys:\n%s", strings.Join(keys, "\n"))
		}
		cfg, err := core.SetConfig(*data, rest[0], rest[1])
		if err != nil {
			return err
		}
		fmt.Printf("%s = %q\npanel %s · TLS %s\nrestart to apply: ivarabase service restart (or restart `ivarabase serve`)\n", rest[0], rest[1], cfg.PublicURL, cfg.TLSMode())
		return nil
	}
	return fmt.Errorf("usage: ivarabase config show|set")
}

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	data := fs.String("data", core.DefaultDataDir(), "data directory")
	fs.Parse(args)
	checks, _ := core.Doctor(*data)
	bad := 0
	for _, c := range checks {
		mark := map[string]string{"ok": "✓", "warn": "!", "error": "✗"}[c.Status]
		fmt.Printf(" %s %-16s %s\n", mark, c.Name, c.Detail)
		if c.Fix != "" && c.Status != "ok" {
			fmt.Printf("   %-16s → %s\n", "", c.Fix)
		}
		if c.Status == "error" {
			bad++
		}
	}
	if bad > 0 {
		return fmt.Errorf("%d problem(s) need fixing", bad)
	}
	return nil
}

// cmdProxyConfig prints the reverse-proxy config for this instance. It reads
// config.json directly, so it works before the server is running.
func cmdProxyConfig(args []string) error {
	fs := flag.NewFlagSet("proxy-config", flag.ExitOnError)
	data := fs.String("data", core.DefaultDataDir(), "data directory")
	server := fs.String("server", "nginx", "nginx or caddy")
	docker := fs.Bool("docker", core.InContainer(), "IvaraBase runs in Docker with 8787 published on the host's 127.0.0.1")
	upstream := fs.String("upstream", "", "address the proxy connects to (default: from listen, e.g. 127.0.0.1:8787)")
	fs.Parse(args)
	cfg, err := core.LoadConfig(*data)
	if err != nil {
		return err
	}
	if *upstream != "" {
		cfg.Listen = *upstream
	}
	if *docker {
		fmt.Fprintln(os.Stderr, "note: for Docker the proxy talks to the published port, so keep `127.0.0.1:8787:8787` under ports: in docker-compose.yml"+
			" (use --upstream if you mapped another port). For Postgres TLS with this certificate, mount the certificate folder and set IVARA_TLS_CERT_FILE / IVARA_TLS_KEY_FILE.")
	}
	conf, err := core.ProxyConfig(cfg, *server)
	if err != nil {
		return err
	}
	fmt.Print(conf)
	if cfg.TLSMode() != "proxy" && !*docker {
		fmt.Fprintf(os.Stderr, "\nnote: tls.mode is %s - run `ivarabase config set tls.mode proxy` and restart so IvaraBase stays off ports 80/443\n", cfg.TLSMode())
	}
	return nil
}

// printService shows what `service install` would write, without touching anything.
func printService(self, data, userName string) error {
	var buf strings.Builder
	if runtime.GOOS == "linux" {
		if userName == "" {
			userName = "ivarabase"
		}
		if data == "" {
			data = "/var/lib/ivarabase"
		}
		home := data
		if u, err := user.Lookup(userName); err == nil && u.HomeDir != "" {
			home = u.HomeDir
		}
		systemdUnit.Execute(&buf, map[string]string{"User": userName, "Group": userName, "Home": home, "Bin": "/usr/local/bin/ivarabase", "Data": data})
		fmt.Printf("# %s\n%s", unitPath, buf.String())
		return nil
	}
	u, err := user.Current()
	if err != nil {
		return err
	}
	if data == "" {
		data = filepath.Join(u.HomeDir, ".ivarabase")
	}
	launchdPlist.Execute(&buf, map[string]string{"Label": launchLabel, "Bin": self, "Data": data, "Home": u.HomeDir, "Log": filepath.Join(data, "logs", "server.log")})
	fmt.Printf("<!-- %s -->\n%s", filepath.Join(u.HomeDir, "Library", "LaunchAgents", launchLabel+".plist"), buf.String())
	return nil
}
