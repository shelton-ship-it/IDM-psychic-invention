#!/usr/bin/env bash
# Compila o daemon Windows de forma reprodutível e gera o ficheiro de somas.
# Uso: scripts/build-windows.sh <versão> [pasta-de-saída]
# Variável opcional GO_MODFILE: ficheiro go.mod alternativo (só para ambientes sem rede).
set -euo pipefail

VERSION="${1:?uso: build-windows.sh <versão> [saída]}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="${2:-$ROOT/dist}"
case "$OUT" in /*) ;; *) OUT="$ROOT/$OUT" ;; esac
COMMIT="$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)"

mkdir -p "$OUT"
cd "$ROOT/core"
EXTRA=()
[ -n "${GO_MODFILE:-}" ] && EXTRA+=("-modfile=$GO_MODFILE")

# -trimpath: sem caminhos da máquina no binário; -s -w: sem símbolos de debug;
# CGO desligado: binário estático, sem dependências de C.
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build "${EXTRA[@]}" -trimpath -buildvcs=false \
  -ldflags "-s -w -X streamvault/core/version.Version=${VERSION} -X streamvault/core/version.Commit=${COMMIT}" \
  -o "$OUT/streamvault-${VERSION}-windows-amd64.exe" ./cmd/streamvault

CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build "${EXTRA[@]}" -trimpath -buildvcs=false \
  -ldflags "-s -w -X streamvault/core/version.Version=${VERSION} -X streamvault/core/version.Commit=${COMMIT}" \
  -o "$OUT/svbench-${VERSION}-windows-amd64.exe" ./cmd/svbench

CGO_ENABLED=0 go build "${EXTRA[@]}" -trimpath -buildvcs=false -o "$OUT/licenseissuer" ./cmd/licenseissuer
# o emissor de licenças NUNCA vai no pacote do cliente: fica fora de dist/release
mkdir -p "$OUT/internal-only" && mv "$OUT/licenseissuer" "$OUT/internal-only/"

cd "$OUT"
sha256sum streamvault-*.exe svbench-*.exe > SHA256SUMS
echo "Gerado em $OUT:"; cat SHA256SUMS
