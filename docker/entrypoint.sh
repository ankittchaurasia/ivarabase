#!/bin/sh
# Container start: checks the data volume, fixes its ownership, copies a
# mounted proxy certificate where the ivarabase user can read it, then runs
# IvaraBase (and through it Postgres, PostgREST and Deno) as that user.
set -eu
BIN=/usr/local/lib/ivarabase/ivarabase
DATA=${IVARA_DATA:-/data}

if [ "$(id -u)" != 0 ]; then
  exec "$BIN" "$@" # started with --user: nothing to fix
fi

if [ "${1:-serve}" = serve ]; then
  if ! mountpoint -q "$DATA" && [ "${IVARA_ALLOW_EPHEMERAL:-}" != 1 ]; then
    cat >&2 <<MSG
IvaraBase: $DATA is not a volume, so every database would be lost when the
container is removed. Mount one, for example:
  docker run -v ivarabase-data:$DATA ...      (or "volumes:" in docker-compose.yml)
Set IVARA_ALLOW_EPHEMERAL=1 to run without one (tests only).
MSG
    exit 1
  fi
  if [ -z "${IVARA_DOMAIN:-}" ] && [ -z "${IVARA_PUBLIC_URL:-}" ]; then
    echo "IvaraBase: IVARA_DOMAIN is not set; the panel is plain HTTP on port 8787 and projects are at /proj/<name>." >&2
  fi
fi

# host folder mounts arrive owned by someone else
if [ "$(stat -c %u "$DATA")" != "$(id -u ivarabase)" ]; then
  chown -R ivarabase:ivarabase "$DATA"
fi
chmod 700 "$DATA"

# Postgres TLS from the proxy's certificate (often root-only on the host):
# copy it into the volume now and whenever it renews.
copy_cert() {
  install -d -o ivarabase -g ivarabase -m 700 "$DATA/tls"
  for pair in "$SRC_CERT:fullchain.pem" "$SRC_KEY:privkey.pem"; do
    src=${pair%%:*} dst="$DATA/tls/${pair##*:}"
    if [ -r "$src" ] && ! cmp -s "$src" "$dst"; then
      cp "$src" "$dst.tmp" && chown ivarabase:ivarabase "$dst.tmp" && chmod 600 "$dst.tmp" && mv "$dst.tmp" "$dst"
    fi
  done
}
if [ -n "${IVARA_TLS_CERT_FILE:-}" ] && [ -n "${IVARA_TLS_KEY_FILE:-}" ]; then
  SRC_CERT=$IVARA_TLS_CERT_FILE SRC_KEY=$IVARA_TLS_KEY_FILE
  case "$SRC_CERT" in "$DATA"/*) ;; *)
    copy_cert
    export IVARA_TLS_CERT_FILE="$DATA/tls/fullchain.pem" IVARA_TLS_KEY_FILE="$DATA/tls/privkey.pem"
    ( while sleep 3600; do copy_cert; done ) &
  esac
fi

exec setpriv --reuid=ivarabase --regid=ivarabase --init-groups -- "$BIN" "$@"
