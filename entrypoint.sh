#!/bin/sh
set -e

if [ -d /entrypoint.d ]; then
  for f in $(find /entrypoint.d -name "*.sh" | sort); do
    [ -f "$f" ] && . "$f"
  done
fi

exec "$@"
