# ADR-009 — HLS sem cifra (vídeo em demanda)

- **Estado:** implementado, ligado ao produto e testado localmente (servidores `httptest`). **Não** validado contra servidores/CDNs reais nem reproduzido em leitores reais (ver "Limites").
- **Contexto:** a captura (ADR-007) listava streams HLS/DASH mas não os descarregava. O ADR-007 apontava isto como a maior lacuna face ao IDM e o ADR-008 recomendou HLS sem cifra como passo seguinte.

## Decisão
- Novo motor `core/hlsengine` para `domain.SourceHLS`, com adaptador `linkrec.NewHLSAdapter` que reconhece URLs `.m3u8`.
- Em `app.New` o adaptador HLS é registado **antes** do HTTP (ordem: torrent, magnet, hls, http); o registry usa a primeira correspondência, por isso, se ficasse depois, o `.m3u8` seria descarregado como um ficheiro de texto.
- `Core.HLS` é fechado em `Core.Close` e nos caminhos de erro de `New`.
- `AddWith` aplica a verificação de segurança do URL e as pistas do navegador (`Referer`/`User-Agent`) a `SourceHTTP` **e** `SourceHLS`.

## O que faz
- **Master playlist:** escolhe a variante de maior débito (desempate por altura).
- **Descarga:** vários segmentos em simultâneo (as ligações da configuração, máximo 16; defeito 6), com uma janela limitada à frente do ponto de escrita para não encher o disco. Os segmentos são escritos **pela ordem da playlist**, mesmo quando terminam fora de ordem.
- **Robustez:** repete segmentos com falha transitória (5 tentativas, recuo progressivo), repete após 30 s sem bytes, e reduz a concorrência se o servidor responder 429/503. Segmento que já não existe (404) ou acesso recusado (401/403/410) falha com explicação.
- **Retoma:** estado em disco (`.svhls-<id>/state.json`); volta ao último ponto sincronizado sem descarregar de novo os segmentos já anexados. Se a playlist mudou, recomeça limpo em vez de misturar versões.
- **Saída:** ficheiro concatenado `.ts`; `.mp4` se a playlist tem `#EXT-X-MAP` (fMP4: o segmento de inicialização vai primeiro); `.aac`/`.mp3` se todos os segmentos o forem. Nome vindo do URL, passado por `security.CheckFileName`.
- **Conclusão:** SHA-256 do que foi escrito, verificação conforme a política de integridade, `rename` atómico sem sobrescrever (nome com sufixo em caso de colisão). Emite `EventFileDiscovered` + `EventCompleted` com `path` e `sha256`, e o índice de hashes (F9) trata-o como qualquer outra tarefa.
- **Limite de velocidade** da tarefa aplicado ao conjunto dos pedidos.

## O que recusa (com erro tipado `unsupported_source` e o que fazer a seguir, sem descarregar segmentos)
- **Cifra** (`#EXT-X-KEY`): o StreamVault não descarrega streams cifrados. Mantém a linha do ADR-002 (fora de scope permanente: conteúdo protegido).
- **Transmissões ao vivo** (playlist sem `#EXT-X-ENDLIST`).
- **`#EXT-X-BYTERANGE`**.
- **Áudio em separado em MPEG-TS** (ver ADR-011: em MP4 fragmentado já é juntado ao vídeo).
- Playlist com mais de 5 MiB, mais de 20 000 segmentos, linhas demasiado longas ou aninhamento de master acima de 3 níveis.

## Segurança
- A playlist vem de terceiros: um URL de segmento ou variante que aponte para a rede interna é bloqueado **quando a playlist veio de uma origem pública**. Só uma playlist já em rede privada (que o utilizador escolheu) pode apontar para a rede privada.
- Redirecionamentos passam pela mesma regra do motor HTTP (`httpengine.CheckRedirect`).
- `Referer`/`User-Agent` são aplicados à playlist e a todos os segmentos e não saem pela API nem pelos logs (igual ao ADR-007).

## Captura e extensão
- `capture`: `.m3u8` (ou tipo MIME `application/vnd.apple.mpegurl`/`application/x-mpegurl`) passa a `Supported: true`. **Atualização (ADR-011):** DASH (`.mpd`, `application/dash+xml`) também passou a `Supported: true`.
- Extensão (`classify.js`): o mesmo critério. A extensão não consegue saber se o stream é cifrado, ao vivo ou tem DRM: nesses casos a tarefa falha no motor com a explicação.

## Verificação
- `go vet` e `go test -race` sem falhas nos pacotes tocados (`hlsengine`, `app`, `capture`); `node --test` na extensão.
- Testes ponta a ponta em `app/hls_test.go`: `Add(.m3u8)` → concluído, conteúdo concatenado na ordem certa, `Referer`/`User-Agent` em **todos** os pedidos, índice de hashes; stream cifrado falha com explicação e sem tocar nos segmentos; playlist em rede privada é bloqueada por defeito sem contactar o servidor.
- Confirmado por mutação que cada ligação em `app.go` é coberta por um teste (segurança, pistas do navegador, ordem dos adaptadores e registo do motor).

## Limites (por desenho ou por verificar)
- **Não testado contra servidores reais.** Os testes usam servidores locais; comportamentos de CDNs (tokens que expiram a meio, limites por IP, playlists não conformes) são por descobrir.
- **A saída é uma concatenação de segmentos, sem remux.** Não foi verificado que cada ficheiro resultante se reproduz em leitores reais; timestamps descontínuos ou ficheiros fMP4 particulares podem exigir remux (fora de âmbito aqui).
- Áudio em separado só em MP4 fragmentado e DASH só estático e sem DRM (ADR-011); sem cifra; sem byte-ranges; sem transmissões ao vivo.
- Cookies: suportados desde o ADR-010, só em memória e só para o host da playlist (segmentos noutro host, como um CDN, vão sem credenciais).
- O motor limita as ligações a 16 mesmo que a configuração permita mais.
- **Não afirmar** em marketing que "descarrega qualquer vídeo": só HLS sem cifra, em demanda. Por isso a funcionalidade `hls` em `site-config.json` fica `enabled: false` até haver um teste com um servidor real (mesma regra do torrent, ver `docs/RELEASE.md`).
