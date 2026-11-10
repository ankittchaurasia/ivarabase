#!/bin/sh
# `docker exec <container> ivarabase ...` runs as root; run the real binary
# as the ivarabase user so nothing in /data ends up owned by root.
if [ "$(id -u)" = 0 ]; then
  exec setpriv --reuid=ivarabase --regid=ivarabase --init-groups -- /usr/local/lib/ivarabase/ivarabase "$@"
fi
exec /usr/local/lib/ivarabase/ivarabase "$@"
