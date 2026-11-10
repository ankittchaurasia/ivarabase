#!/usr/bin/env bash
# Installs what IvaraBase needs on Ubuntu/Debian (amd64 or arm64):
#   PostgreSQL 18 (official PGDG apt repo), PostgREST and Deno at pinned,
#   checksum-verified versions - the versions IvaraBase is tested against.
#   sudo bash scripts/install-deps-ubuntu.sh
#   sudo WITH_POSTGIS=1 bash scripts/install-deps-ubuntu.sh   # also PostGIS (~100 MB)
# pgvector is always installed (small; turned on per project from the UI).
# Safe to re-run: it upgrades PostgREST/Deno to the pinned versions and leaves
# everything else alone. Safe on a busy server: it never stops, removes or
# reconfigures an existing PostgreSQL, and IvaraBase's own cluster uses port
# 54329, not 5432.
set -euo pipefail
[ "$(id -u)" -eq 0 ] || { echo "run with sudo"; exit 1; }
export DEBIAN_FRONTEND=noninteractive

# ---- pinned versions: scripts/deps.env (shared with the Dockerfile) ----------
. "$(dirname "$0")/deps.env"

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# fetch URL FILE SHA256: download and refuse anything that doesn't match
fetch() {
  curl -fsSL --retry 3 "$1" -o "$2"
  echo "$3  $2" | sha256sum -c --quiet - || { echo "!! checksum mismatch for $1 - not installing it"; exit 1; }
}

echo "==> base packages"
apt-get update -qq
apt-get install -y -qq curl ca-certificates gnupg xz-utils unzip lsb-release >/dev/null

if [ ! -x /usr/lib/postgresql/$PG_MAJOR/bin/postgres ]; then
  echo "==> PostgreSQL $PG_MAJOR binaries (apt.postgresql.org)"
  # Reuse the PGDG repo if this server already has it (a second entry with a
  # different signed-by would break apt update).
  if ! grep -rqs "apt.postgresql.org" /etc/apt/sources.list /etc/apt/sources.list.d/; then
    install -d /usr/share/postgresql-common/pgdg
    curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc
    echo "deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc] https://apt.postgresql.org/pub/repos/apt $(lsb_release -cs)-pgdg main" \
      > /etc/apt/sources.list.d/pgdg.list
    apt-get update -qq
  fi
  # IvaraBase runs its own cluster from these binaries. Don't let the package
  # create and start a default "$PG_MAJOR/main" cluster on 5432, and never
  # touch clusters or services that already exist on this machine.
  install -d /etc/postgresql-common/createcluster.d
  echo "create_main_cluster = false" > /etc/postgresql-common/createcluster.d/zz-ivarabase-install.conf
  trap 'rm -f /etc/postgresql-common/createcluster.d/zz-ivarabase-install.conf; rm -rf "$TMP"' EXIT
  apt-get install -y -qq postgresql-$PG_MAJOR >/dev/null
  rm -f /etc/postgresql-common/createcluster.d/zz-ivarabase-install.conf
fi
echo "    $(/usr/lib/postgresql/$PG_MAJOR/bin/postgres --version)"
EXT_PKGS="postgresql-$PG_MAJOR-pgvector"
[ "${WITH_POSTGIS:-}" = 1 ] && EXT_PKGS="$EXT_PKGS postgresql-$PG_MAJOR-postgis-3"
echo "==> extensions: $EXT_PKGS"
apt-get install -y -qq $EXT_PKGS >/dev/null

if [ "$(postgrest --version 2>/dev/null | head -1)" != "PostgREST $POSTGREST_VERSION" ]; then
  echo "==> PostgREST $POSTGREST_VERSION"
  fetch "https://github.com/PostgREST/postgrest/releases/download/v$POSTGREST_VERSION/$POSTGREST_ASSET" "$TMP/postgrest.tar.xz" "$POSTGREST_SHA256"
  tar -xJf "$TMP/postgrest.tar.xz" -C "$TMP"
  install -m 0755 "$TMP/postgrest" /usr/local/bin/postgrest
fi
echo "    $(postgrest --version | head -1)"

if [ "$(/usr/local/bin/deno --version 2>/dev/null | head -1 | cut -d' ' -f2)" != "$DENO_VERSION" ]; then
  echo "==> Deno $DENO_VERSION"
  fetch "https://github.com/denoland/deno/releases/download/v$DENO_VERSION/$DENO_ASSET" "$TMP/deno.zip" "$DENO_SHA256"
  unzip -q -o "$TMP/deno.zip" -d "$TMP"
  install -m 0755 "$TMP/deno" /usr/local/bin/deno
fi
echo "    $(/usr/local/bin/deno --version | head -1)"

echo
echo "Done. Next:"
echo "  sudo install -m 0755 ivarabase-linux-$BIN_ARCH /usr/local/bin/ivarabase"
echo "  ivarabase doctor --data /var/lib/ivarabase"
echo "  sudo ivarabase service install --domain base.example.com --email you@example.com"
echo "  # ports 80/443 already used by nginx/Caddy/Traefik? proxy mode instead (docs/PROXY.md):"
echo "  sudo ivarabase service install --domain base.example.com --proxy --cert-file /path/fullchain.pem --key-file /path/privkey.pem"
