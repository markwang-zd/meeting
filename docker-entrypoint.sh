#!/bin/sh
set -eu

if [ -f /app/env ]; then
  set -a
  . /app/env
  set +a
fi

exec /app/meeting-room-server "$@"
