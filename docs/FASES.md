# Estado das fases (core Go)

Verificado com `go vet` e `go test -race`. Legenda: ✅ feito e testado · ⚠️ feito, com parte por verificar fora deste ambiente · ⏳ por fazer.

| Fase | Estado | Notas |
|---|---|---|
| F1 Contratos/núcleo | ✅ | domain, engine, state machine, config versionada |
| F2 Motor HTTP | ✅ | retoma, pausa pré-escrita, Start após pausa; **aceleração por várias ligações** (ADR-008), ganho real por medir com `svbench` |
| F3 BitTorrent | ⚠️ | motor nativo `btnative` (peers, trackers HTTP/UDP, DHT, magnet) testado contra enxames locais; **falta validar com um magnet real** (ADR-007) |
| F4 Orquestrador+persistência | ⚠️ | `storage` e `integration_sqlite_test.go` só com type-check: correr `go test ./...` com rede |
| F5 Segurança+licenciamento | ✅ | emissor isolado em `cmd/licenseissuer` |
| F6 Windows | ⚠️ | daemon + API + UI testados e cross-compilados; falta assinatura e instalador |
| F7 Android | ⚠️ | fachada `core/mobile` testada; falta AAR (gomobile/NDK) e o PoC do gate (ADR-003) |
| F8 Website | ✅ | `website/`, verificado em 320–1280 px e tema escuro; falta publicar e `checkout_url` |
| F9 Organização | ✅ | `organizer` + `Core.Organize` |
| F10 Handoff/LAN | ✅ | `lan` testado em loopback, com licença e política do núcleo; ficheiros exigem confirmação na UI |
| F11 Plugins | ✅ | `plugin`: aprovação do utilizador e impressão digital |
| F12 Produção | ⚠️ | CI, build reprodutível e guia prontos; faltam assinatura, instalador e publicação (`docs/RELEASE.md`) |

## Política de links (paridade IDM/1DM)
Links funcionam por defeito: servidores locais/NAS incluídos; só um site público a redirecionar para a rede interna é bloqueado. Ver `docs/adr/ADR-006-links-abertos-por-defeito.md`.

## Captura de vídeo (IDM)
`core/capture` + `POST /v1/capture` + extensão em `extension/` (verificada num Chromium real). Streams **HLS** (`.m3u8`) sem cifra e em demanda são descarregados (ver abaixo); **DASH** (`.mpd`) estático e sem DRM também, e vídeo+áudio em separado são juntados sem recodificar (ADR-011).

## Cookies, agendador e grabber de links
⚠️ ADR-010. Cookies (downloads atrás de login) **só em memória**, limitados ao host do link; agendador (`core/scheduler`, `/v1/schedule`); lista de ficheiros de uma página com paginação no mesmo host (`/v1/capture/links`) e lote (`/v1/tasks/batch`); extensão com opção de cookies (opt-in) e botões "Todos os ficheiros" e "Agendar para". Testado com servidores locais; **falta** validar com sites reais com login e a extensão num navegador.

## DASH e áudio em separado
⚠️ ADR-011. `core/mp4merge` (usa `mp4ff`) junta vídeo e áudio fMP4 sem recodificar; `hlsengine` descarrega várias faixas (retoma por faixa) para HLS com `#EXT-X-MEDIA` e para DASH (`SourceDASH`, `.mpd`). Testado com fixtures reais do ffmpeg e validado com `ffprobe`/`ffmpeg`; **falta** validar com servidores/CDNs reais. HLS em MPEG-TS com áudio separado e WebM separado continuam recusados.

## HLS (vídeo em demanda sem cifra)
⚠️ `core/hlsengine` ligado ao núcleo (`Core.HLS`, adaptador antes do HTTP) e à captura/extensão (ADR-009). Testado com servidores locais (ponta a ponta em `core/app/hls_test.go`); **falta validar com servidores reais e reproduzir o resultado em leitores reais**. Recusa, com explicação: cifra, ao vivo, byte-ranges e áudio em separado.

## Contrato de um Backend BitTorrent alternativo (ex.: anacrolix)
Implementar `torrentengine.Backend`/`Session`. Regras que o motor assume:
1. `Add` não grava conteúdo; `Session.Meta` devolve o metainfo bencode sem iniciar download.
2. Só `Session.Download(selected)` inicia a transferência dos ficheiros selecionados.
3. `Session.Stop` para a rede e mantém os dados em disco.
4. Layout em disco: `<destDir>/<name>` (single-file) ou `<destDir>/<name>/<path>` (multi-file).
