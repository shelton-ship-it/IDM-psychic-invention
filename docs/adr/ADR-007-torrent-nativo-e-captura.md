# ADR-007 — Motor torrent nativo e captura de vídeo a partir de páginas

- **Estado:** implementado e testado localmente. **Não** validado contra um enxame público (ver "Limites").
- **Contexto:** paridade com IDM/1DM exige magnet/.torrent e captura de vídeo em páginas. O `anacrolix/torrent` não compila no ambiente de desenvolvimento (rede e versão de Go), pelo que não era verificável.

## Torrent e magnet
- **Decisão:** motor em Go puro (`core/btnative`), atrás de `torrentengine.Backend`. O anacrolix continua possível como alternativa sem mexer em mais nada.
- **O que implementa:** protocolo de peers (BEP 3), extensões (BEP 10), metadados de magnet (BEP 9), trackers HTTP e UDP (BEP 3/15), cliente DHT (BEP 5), retoma por verificação de peças, seleção de ficheiros e upload básico (4 slots).
- **Segurança:**
  - os metadados de um magnet só são aceites se o SHA-1 bater com o info-hash;
  - nada é gravado antes de `Download`;
  - cada peça é verificada (SHA-1) antes de ir para o disco;
  - peers que enviam peças inválidas são expulsos;
  - mensagens acima de 128 KiB são recusadas sem alocar;
  - trackers, peers e nós DHT em redes privadas são ignorados por defeito (`AllowPrivatePeers`), porque vêm de terceiros.
- **Seleção parcial:** quando nenhuma peça fica inteira nos ficheiros escolhidos, o disco não prova nada; o motor aceita a prova do backend (`SelfVerifier`) que verificou cada peça antes de a gravar.
- **Ativo por defeito** no daemon (`-no-torrent`, `-no-dht`, `-bt-listen`, `-bt-lan-peers`) e no Android.
- **Limites (por desenho ou por verificar):** sem uTP, sem cifra de protocolo (MSE/PE), sem PEX, DHT só como cliente, sem IPv6 na DHT; interoperabilidade com clientes reais e com a DHT pública **não foi testada** (os testes usam peers e trackers locais, incluindo peers escritos com bytes literais, independentes do código do motor). Torrents v2 puros são recusados com erro explicado.

## Captura de vídeo
- **Decisão:** duas peças. (1) `core/capture` analisa o HTML (`<video>`/`<audio>`/`<source>`, `og:video`, ligações diretas, JSON-LD `contentUrl`, URLs de mídia em scripts de leitores) e expõe `POST /v1/capture`. (2) Extensão MV3 (`extension/`) que observa respostas de rede e envia o URL com `Referer` e `User-Agent` do navegador.
- **Fora de âmbito (mantém ADR-002):** extratores específicos de sites, DRM, login/paywall alheios. **Cookies:** na altura deste ADR não eram suportados. **Atualização (ADR-010):** passaram a ser, mas só em memória (nunca persistidos) e limitados ao host do link.
- **HLS/DASH:** na altura deste ADR eram listados mas marcados como não suportados. **Atualização (ADR-009):** HLS (`.m3u8`) sem cifra e em demanda passou a ser descarregado pelo motor `hlsengine`. **Atualização (ADR-011):** DASH estático sem DRM e áudio em separado (MP4 fragmentado) também. HLS/DASH cifrado ou ao vivo continuam recusados com explicação.
- **API e segurança:**
  - origens `chrome-extension://`, `moz-extension://` e `safari-web-extension://` são aceites (uma página web não as consegue forjar) mas o token continua obrigatório;
  - o token persiste em `api-token` (0600) e a porta por defeito é 47800, com fallback para uma livre;
  - `Referer` e `User-Agent` ficam na origem da tarefa e nunca saem pela API nem pelos logs.
- **Correção encontrada pelo teste em navegador real:** o motor HTTP não criava a pasta de destino; passou a criá-la e a classificar o erro quando é impossível.
