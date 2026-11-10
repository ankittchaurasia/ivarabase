#!/usr/bin/env bash
# Full self-test on throwaway instances: the supabase-js compatibility suite
# (REST, auth, storage, functions - 52 checks), Sign in with Apple against a
# local mock of Apple's servers (38 checks), and running behind a reverse proxy
# (tls.mode proxy behind an nginx stand-in with a wildcard certificate). Never
# touches ~/.ivarabase, the dev instance or Apple; everything is removed afterwards.
#   make selftest                          # all suites, auto-detected binaries
#   SUITES=apple make selftest             # some suites (compat | features | uploads | realtime | webhooks | apple | proxy)
#   DENO=/path/deno PG_BIN_DIR=/path/bin POSTGREST=/path/postgrest make selftest
#                                          # test specific dependency versions
set -euo pipefail
cd "$(dirname "$0")/.."
B="$PWD/bin/ivarabase"
GOTOOLCHAIN=${GOTOOLCHAIN:-local} go build -o bin/ivarabase ./cmd/ivarabase
[ -d tests/compat/node_modules ] || (cd tests/compat && npm install --no-audit --no-fund --loglevel=error)

SUITES=${SUITES:-compat features uploads realtime webhooks apple proxy}
PORT=${PORT:-8797} MOCKPORT=${MOCKPORT:-8796} PGPORT=${PGPORT:-54337}
PPORT=${PPORT:-8798} PROXYPORT=${PROXYPORT:-8799} PPGPORT=${PPGPORT:-54338}
T=$(mktemp -d "${TMPDIR:-/tmp}/ivb-selftest.XXXXXX")
PIDS=""
cleanup() {
  for p in $PIDS; do kill "$p" 2>/dev/null && wait "$p" 2>/dev/null || true; done
  rm -rf "$T"
}
trap cleanup EXIT INT TERM
has() { [[ " $SUITES " == *" $1 "* ]]; }
json() { node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>console.log(eval("JSON.parse(s)."+process.argv[1])))' "$1"; }

# boot DIR PORT PGPORT PASSWORD EXTRA_CONFIG_JSON [ENV=VAL...]: init + serve a
# throwaway instance; its admin token goes to DIR.token. (Not run in $(...):
# the server must stay a child of this shell so cleanup can stop it.)
boot() {
  local dir=$1 port=$2 pg=$3 pw=$4 extra=$5; shift 5
  mkdir -p "$dir"
  DIR="$dir" PORT_="$port" PG_="$pg" EXTRA="$extra" node -e '
    const c = { listen: `127.0.0.1:${process.env.PORT_}`, postgres: { port: +process.env.PG_, bin_dir: process.env.PG_BIN_DIR || "" },
                deno_path: process.env.DENO || "", postgrest_path: process.env.POSTGREST || "", ...JSON.parse(process.env.EXTRA) };
    require("fs").writeFileSync(process.env.DIR + "/config.json", JSON.stringify(c, null, 2));'
  "$B" init --data "$dir" --user owner --password "$pw" >/dev/null
  env "$@" "$B" serve --data "$dir" >"$dir/../$(basename "$dir").log" 2>&1 &
  local pid=$!
  PIDS="$PIDS $pid"
  for _ in $(seq 1 60); do
    curl -fsS "http://127.0.0.1:$port/healthz" >/dev/null 2>&1 && break
    kill -0 "$pid" 2>/dev/null || { cat "$dir/../$(basename "$dir").log" >&2; exit 1; }
    sleep 0.5
  done
  "$B" token create --data "$dir" --name selftest --level admin --allow-destructive | json token >"$dir.token"
}

if has compat || has apple || has features || has uploads || has realtime || has webhooks; then
  openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$T/AuthKey_TEST.p8" 2>/dev/null
  node tests/compat/apple-mock.mjs "$MOCKPORT" "$T/AuthKey_TEST.p8" >"$T/mock.log" 2>&1 & PIDS="$PIDS $!"
  boot "$T/data" "$PORT" "$PGPORT" "$(openssl rand -hex 16)" '{}' IVARA_DEV_APPLE_URL="http://127.0.0.1:$MOCKPORT" \
    IVARA_WEBHOOKS_ALLOW_PRIVATE=1 IVARA_DEV_WEBHOOK_RETRY_SCALE=0.02 # webhook tests deliver to a sink on 127.0.0.1
  TOKEN=$(cat "$T/data.token")
  IVARA_URL="http://127.0.0.1:$PORT" IVARA_TOKEN="$TOKEN" "$B" system info | node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>{const i=JSON.parse(s);
    console.log("==> throwaway instance: PostgreSQL", i.postgres.version, "| Deno", (i.deno && i.deno.version) || "not found")})'
  echo "    $("${POSTGREST:-postgrest}" --version 2>/dev/null | head -1 || echo "PostgREST not found")"
fi

if has proxy; then
  # a wildcard certificate like the one a proxy (certbot, a panel) would hold for the proxy
  mkdir -p "$T/cert"
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 30 -subj /CN=ivara.test \
    -addext "subjectAltName=DNS:ivara.test,DNS:*.ivara.test" -keyout "$T/cert/privkey.pem" -out "$T/cert/fullchain.pem" 2>/dev/null
  PPW=$(openssl rand -hex 16)
  boot "$T/pdata" "$PPORT" "$PPGPORT" "$PPW" "$(CERT="$T/cert" PROXYPORT="$PROXYPORT" node -e 'console.log(JSON.stringify({
      domain: "ivara.test", public_url: `https://ivara.test:${process.env.PROXYPORT}`,
      tls: { mode: "proxy", cert_file: process.env.CERT + "/fullchain.pem", key_file: process.env.CERT + "/privkey.pem" } }))')"
  PTOKEN=$(cat "$T/pdata.token")
fi

status=0
for s in $SUITES; do
  case $s in
    compat) echo "==> supabase-js compatibility suite"
            IVARA_URL="http://127.0.0.1:$PORT" IVARA_TOKEN="$TOKEN" ./scripts/compat.sh | tail -3 || status=1 ;;
    realtime) echo "==> realtime (broadcast, presence, postgres_changes, RLS)"
            IV="http://127.0.0.1:$PORT" TOKEN="$TOKEN" node tests/compat/features-realtime.mjs | { if [ -n "${VERBOSE:-}" ]; then cat; else grep -E "^(FAIL|SKIP)|passed"; fi; } || status=1 ;;
    apple)  echo "==> Sign in with Apple"
            IV="http://127.0.0.1:$PORT" TOKEN="$TOKEN" MOCK="http://127.0.0.1:$MOCKPORT" P8="$T/AuthKey_TEST.p8" node tests/compat/apple.mjs | tail -1 || status=1 ;;
    features) echo "==> v0.2 features (extensions, webhooks, auth, uploads, realtime)"
            IV="http://127.0.0.1:$PORT" TOKEN="$TOKEN" node tests/compat/features.mjs | { if [ -n "${VERBOSE:-}" ]; then cat; else grep -E "^(FAIL|SKIP)|passed"; fi; } || status=1 ;;
    uploads) echo "==> resumable uploads (TUS, tus-js-client)"
            IV="http://127.0.0.1:$PORT" TOKEN="$TOKEN" node tests/compat/features-uploads.mjs | { if [ -n "${VERBOSE:-}" ]; then cat; else grep -E "^(FAIL|SKIP)|passed"; fi; } || status=1 ;;
    webhooks) echo "==> database webhooks"
            IV="http://127.0.0.1:$PORT" TOKEN="$TOKEN" node tests/compat/features-webhooks.mjs | { if [ -n "${VERBOSE:-}" ]; then cat; else grep -E "^(FAIL|SKIP)|passed"; fi; } || status=1 ;;
    proxy)  echo "==> behind a reverse proxy (tls.mode proxy)"
            PSQL="${PG_BIN_DIR:+$PG_BIN_DIR/}psql"
            IV="http://127.0.0.1:$PPORT" TOKEN="$PTOKEN" OWNER_PW="$PPW" PROXYPORT="$PROXYPORT" CERT="$T/cert" BIN="$B" DATA="$T/pdata" \
              PSQL="$PSQL" node tests/compat/proxy.mjs | { if [ -n "${VERBOSE:-}" ]; then cat; else grep -E "^FAIL|passed"; fi; } || status=1 ;;
    *) echo "unknown suite $s (compat | features | uploads | realtime | webhooks | apple | proxy)"; status=1 ;;
  esac
done
exit $status
