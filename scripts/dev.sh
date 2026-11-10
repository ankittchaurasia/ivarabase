#!/usr/bin/env bash
# Run the Go server and the Vite UI together for development.
#   UI (hot reload): http://localhost:5173   API/MCP: http://127.0.0.1:8787
# Uses a separate dev instance (./.dev-data) with the fixed login from
# scripts/dev.env, so it never touches a real install in ~/.ivarabase.
# Ctrl+C stops both.
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/dev.env
[ -f scripts/dev.local.env ] && source scripts/dev.local.env

GOTOOLCHAIN=${GOTOOLCHAIN:-local} go build -o bin/ivarabase ./cmd/ivarabase
[ -d web/ui/node_modules ] || (cd web/ui && npm install --no-audit --no-fund)

if [ ! -f "$IVARA_DEV_DATA/pg/PG_VERSION" ]; then
  mkdir -p "$IVARA_DEV_DATA"
  [ -f "$IVARA_DEV_DATA/config.json" ] || cat > "$IVARA_DEV_DATA/config.json" <<JSON
{ "listen": "127.0.0.1:8787", "postgres": { "port": $IVARA_DEV_PG_PORT } }
JSON
  echo "==> creating dev instance in $IVARA_DEV_DATA (login: see scripts/dev.env)"
  ./bin/ivarabase init --data "$IVARA_DEV_DATA" --user "$IVARA_DEV_USER" --password "$IVARA_DEV_PASSWORD" >/dev/null
fi

./bin/ivarabase serve --data "$IVARA_DEV_DATA" "$@" &
SERVER=$!
trap 'kill $SERVER 2>/dev/null; wait $SERVER 2>/dev/null' EXIT INT TERM

for _ in $(seq 1 60); do
  curl -fsS http://127.0.0.1:8787/healthz >/dev/null 2>&1 && break
  kill -0 $SERVER 2>/dev/null || { echo "ivarabase serve exited - see the error above"; exit 1; }
  sleep 0.5
done
echo "==> dev server up. Sign in at http://localhost:5173 with the login in scripts/dev.env"

cd web/ui && npx vite
