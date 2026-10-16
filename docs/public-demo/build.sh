#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../.."
GOOS=js GOARCH=wasm go build -trimpath -o docs/public-demo/engine.wasm ./cmd/public-demo
cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" docs/public-demo/wasm_exec.js
