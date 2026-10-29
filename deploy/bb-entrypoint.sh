#!/bin/sh
set -eu
umask 077

# This optional file belongs only to BB. Never mount Git/deployment credentials
# here, and never import the operator's host home or shell environment.
if [ -n "${OPENROUTER_API_KEY_FILE:-}" ]; then
  if [ ! -f "$OPENROUTER_API_KEY_FILE" ] || [ ! -r "$OPENROUTER_API_KEY_FILE" ]; then
    printf '%s\n' 'BB: OPENROUTER_API_KEY_FILE must be a readable regular file.' >&2
    exit 1
  fi
  OPENROUTER_API_KEY=$(cat "$OPENROUTER_API_KEY_FILE")
  if [ -z "$OPENROUTER_API_KEY" ]; then
    printf '%s\n' 'BB: the OpenRouter secret file is empty; refusing to start.' >&2
    exit 1
  fi
  export OPENROUTER_API_KEY
fi

# The default CMD supervises BB startup and supported OpenFactory plugin install.
# Arbitrary commands still exec directly after the same secret injection.
exec "$@"
