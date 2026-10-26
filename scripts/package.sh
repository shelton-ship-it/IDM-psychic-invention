#!/usr/bin/env bash
# Compila e empacota uma versão: daemon + svbench para cada sistema, a extensão do
# navegador e o ficheiro de somas SHA-256. É o que o GitHub Actions corre
# (.github/workflows/release.yml), mas serve igualmente na sua máquina.
#
# Uso:  scripts/package.sh <versão> [pasta-de-saída]       ex.: scripts/package.sh 1.0.0
# Variáveis opcionais:
#   TARGETS     lista "os/arch" separada por espaços (defeito: windows/amd64 linux/amd64
#               linux/arm64 darwin/amd64 darwin/arm64)
#   GO_MODFILE  go.mod alternativo (só para ambientes sem rede; ver scripts/build-windows.sh)
#
# O emissor de licenças (licenseissuer) NUNCA é empacotado: só o cliente sai daqui.
# Reprodutível: -trimpath, sem símbolos de debug, CGO desligado, ficheiros com data fixa
# (a do commit), ordem fixa nos arquivos e gzip sem data. Os mesmos fontes dão os mesmos
# arquivos byte a byte (a verificar com SHA256SUMS).
set -euo pipefail

VERSION="${1:?uso: package.sh <versão> [pasta-de-saída]}"
# aceita 1.2.3, 1.2.3-rc1, 1.2.3+build (sem espaços nem barras: vai para nomes de ficheiros)
if ! [[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$ ]]; then
  echo "versão inválida: '$VERSION' (use por exemplo 1.0.0 ou 1.0.0-rc1)" >&2
  exit 2
fi
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="${2:-$ROOT/dist}"
case "$OUT" in /*) ;; *) OUT="$PWD/$OUT" ;; esac
COMMIT="$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)"
SDE="${SOURCE_DATE_EPOCH:-$(git -C "$ROOT" log -1 --format=%ct 2>/dev/null || echo 0)}"
TARGETS="${TARGETS:-windows/amd64 linux/amd64 linux/arm64 darwin/amd64 darwin/arm64}"
EXTRA=()
[ -n "${GO_MODFILE:-}" ] && EXTRA+=("-modfile=$GO_MODFILE")

rm -rf "$OUT"
mkdir -p "$OUT/stage"

readme() { # $1 = sistema
  cat <<EOF
StreamVault ${VERSION} (${COMMIT})
=========================================

Como usar
---------
1. Execute o StreamVault:
     $( [ "$1" = windows ] && echo 'streamvault.exe -open' || echo './streamvault -open' )
   Abre a interface no navegador. O programa só escuta no seu próprio computador (127.0.0.1).
2. Para capturar vídeos das páginas, instale a extensão (ficheiro
   streamvault-extension-${VERSION}.zip, descompacte-o): no Chrome/Edge abra
   chrome://extensions, ative o "Modo de programador" e escolha "Carregar sem
   compactação". Nas Definições da extensão indique o endereço e a chave de acesso
   que o StreamVault mostra ao arrancar.
3. "streamvault -help" lista as opções (pasta de downloads, torrent, etc.).

Importante
----------
- Este ficheiro NÃO está assinado digitalmente$( [ "$1" = windows ] && echo ': o Windows (SmartScreen) pode avisar que o editor é desconhecido' ). Confirme a
  integridade com o ficheiro SHA256SUMS publicado com a versão.
- svbench mede a velocidade de descarga com N ligações (útil para comparar com
  outros gestores no mesmo ficheiro e rede).
- Esta versão não tem atualização automática.
EOF
}

build_one() { # $1 = os, $2 = arch
  local os="$1" arch="$2" ext="" name stage cmd
  [ "$os" = windows ] && ext=".exe"
  name="streamvault-${VERSION}-${os}-${arch}"
  stage="$OUT/stage/$name"
  mkdir -p "$stage"
  for cmd in streamvault svbench; do
    (cd "$ROOT/core" && CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build ${EXTRA[@]+"${EXTRA[@]}"} -trimpath -buildvcs=false \
      -ldflags "-s -w -X streamvault/core/version.Version=${VERSION} -X streamvault/core/version.Commit=${COMMIT}" \
      -o "$stage/${cmd}${ext}" "./cmd/${cmd}")
  done
  cp "$ROOT/LICENSE" "$stage/LICENSE"
  readme "$os" > "$stage/LEIA-ME.txt"
  find "$stage" -exec touch -d "@$SDE" {} +
  if [ "$os" = windows ]; then
    (cd "$OUT/stage" && find "$name" -type f | LC_ALL=C sort | zip -X -q "$OUT/$name.zip" -@)
  else
    tar -C "$OUT/stage" --sort=name --mtime="@$SDE" --owner=0 --group=0 --numeric-owner \
      -cf - "$name" | gzip -n -9 > "$OUT/$name.tar.gz"
  fi
  echo "  ok  $name"
}

echo "A compilar $VERSION ($COMMIT) para: $TARGETS"
for t in $TARGETS; do
  build_one "${t%/*}" "${t#*/}"
done

# ---- extensão do navegador (JS puro: não há compilação, só empacotar) ----
EXT="streamvault-extension-${VERSION}"
rm -rf "$OUT/stage/$EXT"
mkdir -p "$OUT/stage/$EXT"
(cd "$ROOT/extension" && tar -cf - --exclude=./test --exclude=./node_modules --exclude=./package.json .) | tar -xf - -C "$OUT/stage/$EXT"
# a loja exige 1 a 4 números separados por pontos: usa a versão sem sufixo de pré-lançamento
MV="${VERSION%%[-+]*}"
python3 - "$OUT/stage/$EXT/manifest.json" "$MV" <<'PY'
import json, sys
p, v = sys.argv[1], sys.argv[2]
m = json.load(open(p, encoding="utf-8"))
m["version"] = v
json.dump(m, open(p, "w", encoding="utf-8"), ensure_ascii=False, indent=2)
open(p, "a", encoding="utf-8").write("\n")
PY
find "$OUT/stage/$EXT" -exec touch -d "@$SDE" {} +
(cd "$OUT/stage" && find "$EXT" -type f | LC_ALL=C sort | zip -X -q "$OUT/$EXT.zip" -@)
echo "  ok  $EXT (manifest ${MV})"

# ---- somas ----
rm -rf "$OUT/stage"
cd "$OUT"
sha256sum -- *.zip *.tar.gz 2>/dev/null > SHA256SUMS || sha256sum -- *.zip > SHA256SUMS
echo
echo "Gerado em $OUT:"
cat SHA256SUMS
