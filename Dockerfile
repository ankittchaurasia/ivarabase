# IvaraBase in one container: the ivarabase server plus its own PostgreSQL,
# PostgREST and Deno. Runs behind the reverse proxy the server already has
# (nginx, Caddy, Traefik, or a hosting panel): tls.mode=proxy.
#   docker build -t ivarabase .        (or: make docker)
# Pinned dependency versions come from scripts/deps.env, shared with
# scripts/install-deps-ubuntu.sh.

ARG PG_MAJOR=18

# ---- build the static Go binary (the web UI is prebuilt in web/static)
FROM --platform=$BUILDPLATFORM golang:1.23-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X ivarabase/internal/core.Version=$VERSION" -o /out/ivarabase ./cmd/ivarabase

# ---- runtime
FROM debian:trixie-slim
ARG PG_MAJOR
ARG TARGETARCH
ENV DEBIAN_FRONTEND=noninteractive
COPY scripts/deps.env /tmp/deps.env
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl gnupg xz-utils unzip tini; \
    install -d /usr/share/postgresql-common/pgdg; \
    curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc; \
    . /etc/os-release; \
    echo "deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc] https://apt.postgresql.org/pub/repos/apt $VERSION_CODENAME-pgdg main" > /etc/apt/sources.list.d/pgdg.list; \
    install -d /etc/postgresql-common/createcluster.d; \
    echo "create_main_cluster = false" > /etc/postgresql-common/createcluster.d/no-main.conf; \
    apt-get update; \
    apt-get install -y --no-install-recommends postgresql-$PG_MAJOR postgresql-$PG_MAJOR-pgvector; \
    ARCH=${TARGETARCH:-$(dpkg --print-architecture)} . /tmp/deps.env; \
    cd /tmp; \
    curl -fsSL -o postgrest.tar.xz "https://github.com/PostgREST/postgrest/releases/download/v$POSTGREST_VERSION/$POSTGREST_ASSET"; \
    echo "$POSTGREST_SHA256  postgrest.tar.xz" | sha256sum -c -; \
    tar -xJf postgrest.tar.xz -C /usr/local/bin postgrest; \
    curl -fsSL -o deno.zip "https://github.com/denoland/deno/releases/download/v$DENO_VERSION/$DENO_ASSET"; \
    echo "$DENO_SHA256  deno.zip" | sha256sum -c -; \
    unzip -q deno.zip -d /usr/local/bin; \
    chmod 755 /usr/local/bin/postgrest /usr/local/bin/deno; \
    apt-get purge -y --auto-remove curl gnupg xz-utils unzip; \
    rm -rf /var/lib/apt/lists/* /tmp/* /usr/share/doc /usr/share/man; \
    groupadd --system --gid 10001 ivarabase; \
    useradd --system --uid 10001 --gid 10001 --home-dir /data --shell /usr/sbin/nologin ivarabase; \
    install -d -o ivarabase -g ivarabase -m 700 /data

COPY --from=build /out/ivarabase /usr/local/lib/ivarabase/ivarabase
COPY docker/entrypoint.sh /usr/local/lib/ivarabase/entrypoint.sh
COPY docker/ivarabase-wrapper.sh /usr/local/bin/ivarabase
RUN chmod 755 /usr/local/lib/ivarabase/* /usr/local/bin/ivarabase

ENV IVARA_IN_CONTAINER=1 \
    IVARA_DATA=/data \
    IVARA_LISTEN=0.0.0.0:8787 \
    IVARA_TLS_MODE=proxy \
    IVARA_TRUSTED_PROXIES=127.0.0.0/8,::1/128,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,fc00::/7 \
    IVARA_PG_BIN_DIR=/usr/lib/postgresql/$PG_MAJOR/bin \
    IVARA_PG_LISTEN=* \
    IVARA_DENO_PATH=/usr/local/bin/deno \
    IVARA_POSTGREST_PATH=/usr/local/bin/postgrest \
    DENO_NO_UPDATE_CHECK=1

EXPOSE 8787 54329
STOPSIGNAL SIGTERM
HEALTHCHECK --interval=30s --timeout=6s --start-period=60s --retries=3 CMD ["/usr/local/bin/ivarabase", "healthcheck"]
ENTRYPOINT ["/usr/bin/tini", "-g", "--", "/usr/local/lib/ivarabase/entrypoint.sh"]
CMD ["serve"]
