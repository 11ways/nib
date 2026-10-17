#!/bin/sh
set -eu

if ! command -v go >/dev/null 2>&1; then
  printf '%s\n' 'Go is required to build nib. Install Go, then run this script again.' >&2
  exit 1
fi

export GOBIN="${GOBIN:-$HOME/.local/bin}"
printf 'Installing nib to %s/nib...\n' "$GOBIN"
go -C "$(dirname "$0")" install ./cmd/nib
"$GOBIN/nib" --version

if [ "$(command -v nib || true)" != "$GOBIN/nib" ]; then
  printf 'Put %s first on PATH to use this installation.\n' "$GOBIN"
fi
printf '%s\n' 'Restart your nib MCP server or client to load the new binary.'
