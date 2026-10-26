# ADR-011 — DASH e áudio em separado (juntar sem recodificar)

- **Estado:** implementado e testado com **fixtures reais gerados pelo ffmpeg** (HLS fMP4 e um `manifest.mpd` do próprio ffmpeg) e com validação **independente** do resultado pelo `ffprobe`/`ffmpeg` (descodificam sem erros; vídeo h264 + áudio aac). **Não** validado contra servidores/CDNs reais nem em leitores reais de utilizador. O ffmpeg **não** é dependência do produto (só é usado para gerar fixtures e, se existir, nos testes).
- **Contexto:** o ADR-009 recusava HLS com `#EXT-X-MEDIA` de áudio (comum em HLS reais) e o DASH nem era descarregado. Juntar vídeo e áudio sem ffmpeg parecia um muxer escrito à mão; uma biblioteca madura de MP4 torna o remux sem perdas pequeno e verificável, e funciona no Android (Go puro).

## Decisão
1. **`core/mp4merge`** (novo): junta ficheiros MP4 fragmentado de uma faixa cada num só MP4 fragmentado, **sem recodificar**. Usa `github.com/Eyevinn/mp4ff` **v0.50.0** (MIT, sem dependências de execução; é a mais recente que compila com o Go 1.22 do projeto: as seguintes exigem Go 1.24).
2. **`hlsengine`** passa a descarregar **várias faixas** (cada uma com o descarregador, a janela, o backoff, o limite de velocidade e a retoma que já existiam) e a juntá-las no fim. Serve HLS **e** DASH (o nome do pacote ficou por compatibilidade).
3. **HLS:** a variante escolhida usa a faixa de áudio do seu `GROUP-ID` (`DEFAULT=YES`, ou a primeira). **DASH:** novo `domain.SourceDASH`, adaptador `linkrec.NewDASHAdapter` (`.mpd`, antes do HTTP) e interpretador de MPD com `encoding/xml` da stdlib (o Go não expande entidades externas e o formato precisa de ~300 linhas, por isso não se acrescentou outra biblioteca).

## Como junta (mp4merge)
- Cada entrada é um fMP4 completo (inicialização + fragmentos), uma faixa por ficheiro. As faixas ficam com os ids 1, 2… pela ordem dada.
- Escreve `ftyp` + `moov` combinado (traks e `trex` de todas; `mehd` removido) e depois os fragmentos **intercalados por tempo de descodificação** (`tfdt`/escala da faixa), renumerando `mfhd.SequenceNumber` e o `TrackID` do fragmento. As amostras não são tocadas (confirmado byte a byte nos testes).
- As listas de edição (`elst`) estão na escala do *filme*: se os `mvhd` diferirem, convertem-se.
- Em fluxo: memória ≈ um fragmento por faixa, não o ficheiro inteiro.
- **Recusa, com erro explicado:** não fragmentado (sem `mvex`), várias faixas num só ficheiro, `pssh`/`senc` (protegido), `base-data-offset` absoluto, fragmento sem `tfdt`, ficheiro truncado ou ilegível.

## Motor com várias faixas
- Cada faixa tem a sua pasta de trabalho (`.svhls-<id>/t<N>/`), o seu estado e o seu ficheiro parcial; **pausa/retoma por faixa** (uma faixa já concluída não é descarregada outra vez). O `Done` no estado é uma salvaguarda; o que garante não repetir é o ponto sincronizado guardado (testado: perder o estado por faixa faz o teste falhar).
- Progresso acumulado entre faixas; **enquanto houver faixas por descarregar o total é desconhecido (`-1`)**, em vez de um palpite errado. No fim, `bytesDone == bytesTotal == tamanho do ficheiro`.
- O SHA-256 do evento de conclusão é o do ficheiro **final** (já juntado). A política de integridade (`RequireIntegrity`) aplica-se a esse ficheiro.
- O resultado com 2 faixas é `.mp4`. Uma só faixa continua a ser a concatenação exata de inicialização + segmentos (`.mp4`, `.m4a` só áudio, `.webm`).
- Falha ao juntar: erro `unsupported_source` com a causa, e nenhum ficheiro final deixado.

## DASH suportado
`type="static"` (VOD), **um** período, sem `ContentProtection`, contentores MP4 ou WebM. Endereçamento:
- `SegmentTemplate` com `$Number$`, `$Time$`, `$Bandwidth$`, `$RepresentationID$` (e `%0Nd`), `SegmentTimeline` (`t`, `d`, `r`, incluindo `r` negativo até ao fim do período) ou `duration` + duração do período; modelos herdados do período/adaptação/representação.
- `SegmentList` e **ficheiro único** (`BaseURL`, perfil on-demand).
- `BaseURL` encadeado (MPD → período → adaptação → representação).
- Escolhe o **vídeo de maior débito** e o **áudio** do `AdaptationSet` com `Role=main` (na falta, o primeiro), dentro dele o de maior débito.

## Recusado, com explicação e sem descarregar segmentos
DRM/cifra (`ContentProtection` em qualquer nível, `#EXT-X-KEY`), ao vivo (`type="dynamic"`, sem `#EXT-X-ENDLIST`), vários períodos (típico de publicidade inserida), intervalos de bytes (`mediaRange`, `#EXT-X-BYTERANGE`), contentores que não sejam MP4/WebM (ex.: MPEG-TS em DASH), identificadores de modelo desconhecidos, esquemas que não sejam http(s), mais de 20 000 segmentos, MPD/playlist com mais de 5 MiB.

## Segurança
- Todos os URLs do manifesto passam pela mesma proteção SSRF (`urlGuard`) que o HLS: um manifesto de origem pública não pode apontar para a rede interna.
- O manifesto e os segmentos levam o `Referer`/`User-Agent` e, se houver, o cookie **só do host do manifesto** (ADR-010); segmentos noutro host (CDN) vão sem credenciais.
- XML: o decoder da stdlib não expande entidades externas (sem XXE); tamanho limitado.

## Limites (por desenho ou por verificar)
- **Áudio em separado só em MP4 fragmentado.** HLS em MPEG-TS com áudio em separado (`.aac`/`.ts`) e WebM em separado continuam **recusados** (juntar exigiria um muxer de TS/Matroska).
- Uma só faixa de áudio (a principal/predefinida); **sem escolha de idioma**, sem legendas, sem áudio descritivo.
- Várias qualidades: sempre a de maior débito (sem seletor).
- **Não verificado em servidores reais** (tokens que expiram, CDNs com limites, manifestos não conformes) nem a sincronização áudio/vídeo em leitores reais além do que o ffmpeg decodifica sem erros.
- Fragmentos CMAF sem `tfdt` ou com `base-data-offset` absoluto: recusados (não encontrados nos fixtures; podem existir na natureza).
- Sem `sidx`/`mfra` no resultado (descartados): o ficheiro reproduz-se e descodifica-se, mas a procura (seek) em leitores sem índice pode ser menos eficiente.
- O resultado é MP4 **fragmentado** (não "faststart" tradicional); alguns editores antigos podem exigir convertê-lo.

## Verificação
- `mp4merge`: amostras idênticas byte a byte por faixa, ordem temporal, `mfhd` 1..N, ids de faixa, intercalação real; `ffprobe` (2 streams, ~6 s, ≥100 frames) e `ffmpeg -f null` sem erros; erros explicados (não fragmentado, 2 faixas, truncado, lixo, vazio).
- `hlsengine`: HLS com áudio separado → 1 MP4 reproduzível; DASH real do ffmpeg → 1 MP4 reproduzível (8 pedidos `.m4s`); retoma entre faixas sem repetir a concluída; recusas (TS+áudio, DRM, ao vivo, vários períodos, XML inválido); falha ao juntar explicada e sem ficheiro final. Interpretador de MPD: 7 modos de endereçamento e 18 casos de recusa/entrada inválida.
- Mutações confirmadas (reverter o código faz um teste falhar): intercalação por tempo, renumeração do `TrackID`, retoma por faixa, DRM, número de repetições do `SegmentTimeline`, só uma faixa, registo do motor/adaptador DASH, segurança para DASH. Uma mutação ("repetir faixa concluída") era **equivalente** (executar uma faixa já anexada não faz nada), o que ficou documentado acima em vez de se fingir cobertura.
- Um defeito real foi apanhado pelos testes: a pasta de trabalho `.svhls-<id>` ficava no fim porque contém subpastas por faixa; corrigido.
