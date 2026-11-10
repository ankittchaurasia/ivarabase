package core

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

// Running behind a reverse proxy (tls.mode proxy): nginx, Caddy, Traefik or a
// hosting panel terminates HTTPS for <domain> and *.<domain> and forwards
// everything to the plain listener. ProxyConfig prints a working config so
// nobody has to rediscover the settings that matter: keep the Host header
// (projects are routed by subdomain), pass the client address and scheme,
// don't cap or buffer bodies (IvaraBase enforces its own limits and streams
// function responses), and allow function-length timeouts.

// fnMaxTimeoutS is the longest a function may run (timeout_s 5-300).
const fnMaxTimeoutS = 300

// upstreamAddr is where the proxy should send traffic: the listen address,
// with a wildcard host turned into loopback.
func (c *Config) upstreamAddr() string {
	host, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return c.Listen
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// ProxyConfig renders a reverse-proxy config for server "nginx" (also what
// most hosting panels run) or "caddy".
func ProxyConfig(cfg *Config, server string) (string, error) {
	d := cfg.Domain
	if d == "" {
		return "", fmt.Errorf("set a domain first: ivarabase config set domain base.example.com")
	}
	up := cfg.upstreamAddr()
	timeout := fmt.Sprintf("%ds", fnMaxTimeoutS+10)
	switch server {
	case "", "nginx", "aapanel":
		crt, key := cfg.TLS.CertFile, cfg.TLS.KeyFile
		if crt == "" || key == "" {
			crt, key = "/path/to/fullchain.pem", "/path/to/privkey.pem"
		}
		return fmt.Sprintf(`# IvaraBase behind nginx: panel https://%[1]s, projects https://<project>.%[1]s
# Needs DNS A records for %[1]s and *.%[1]s, and a certificate covering both
# (a wildcard certificate needs a DNS challenge, e.g. certbot with a DNS plugin).
#
# Hosting panels that manage nginx: create a static site (no PHP) for both
# names, add the certificate, and paste the "location /" block into the site's
# server { }. Remove any static-file location blocks the panel added (for
# .js/.css/images): they answer before the proxy and break the dashboard.

server {
    listen 443 ssl http2;
    listen [::]:443 ssl http2;
    server_name %[1]s *.%[1]s;

    ssl_certificate     %[2]s;
    ssl_certificate_key %[3]s;

    location / {
        proxy_pass http://%[4]s;
        proxy_http_version 1.1;
        # realtime (supabase-js channels) is a WebSocket: pass the upgrade on
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $http_connection;

        # projects are routed by subdomain: keep the original Host
        proxy_set_header Host $host;
        # real visitor address and scheme (trusted from loopback only)
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;

        # IvaraBase enforces upload limits itself (storage max_file_size_mb,
        # per-bucket limits); stream bodies instead of spooling them to disk
        client_max_body_size 0;
        proxy_request_buffering off;
        # streamed function responses arrive as they are written
        proxy_buffering off;
        # functions may run up to %[6]d seconds
        proxy_read_timeout %[5]s;
        proxy_send_timeout %[5]s;
    }
}

server {
    listen 80;
    listen [::]:80;
    server_name %[1]s *.%[1]s;
    return 301 https://$host$request_uri;
}

# Postgres doesn't go through nginx: clients connect to db.%[1]s:%[7]d
# directly. For TLS on those connections point IvaraBase at the same files:
#   ivarabase config set tls.cert_file %[2]s
#   ivarabase config set tls.key_file %[3]s
`, d, crt, key, up, timeout, fnMaxTimeoutS, cfg.Postgres.Port), nil
	case "caddy":
		return fmt.Sprintf(`# IvaraBase behind Caddy: panel https://%[1]s, projects https://<project>.%[1]s
# Needs DNS A records for %[1]s and *.%[1]s.
#
# Certificates: on-demand TLS gets one per hostname as it is first visited,
# and IvaraBase answers the "ask" check so only the panel, db.%[1]s and
# existing projects get one. (Prefer a wildcard? Replace "tls { on_demand }"
# with "tls { dns <provider> ... }" using a Caddy DNS plugin.)

{
    on_demand_tls {
        ask http://%[2]s/_tls/ask
    }
}

%[1]s, *.%[1]s {
    tls {
        on_demand
    }
    reverse_proxy %[2]s {
        # Caddy already sends Host, X-Forwarded-For and X-Forwarded-Proto
        flush_interval -1
        transport http {
            read_timeout %[3]s
            write_timeout %[3]s
        }
    }
}

# Postgres doesn't go through Caddy: clients connect to db.%[1]s:%[4]d directly.
# For TLS there, give IvaraBase a certificate covering db.%[1]s:
#   ivarabase config set tls.cert_file /path/fullchain.pem
#   ivarabase config set tls.key_file /path/privkey.pem
`, d, up, timeout, cfg.Postgres.Port), nil
	}
	return "", fmt.Errorf("server must be nginx or caddy")
}

// handleTLSAsk answers Caddy's on-demand TLS "ask": 200 only for names we
// serve, so random hostnames pointed at the server can't burn certificate
// rate limits. Only the proxy itself may ask: a request it forwards for a
// visitor carries X-Forwarded-For / X-Real-IP and is refused.
func (s *Server) handleTLSAsk(w http.ResponseWriter, r *http.Request) {
	forwarded := r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("X-Real-IP") != "" || r.Header.Get("Forwarded") != ""
	if _, peer := peerIP(r); !s.app.Cfg.isTrustedProxy(peer) || forwarded {
		http.NotFound(w, r)
		return
	}
	host := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("domain")))
	if host == "" || s.app.Cfg.Domain == "" || s.app.hostPolicy(r.Context(), host) != nil {
		http.Error(w, "no", http.StatusForbidden)
		return
	}
	w.Write([]byte("ok\n"))
}
