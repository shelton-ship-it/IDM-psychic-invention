# O que falta (entrega para continuar noutro chat)

## Estado ao parar (24/10)
- **HLS sem cifra** (ADR-009), **cookies / agendador / grabber** (ADR-010) e agora **DASH + áudio em separado** (ADR-011):
  - `core/mp4merge` (novo, usa `github.com/Eyevinn/mp4ff v0.50.0`): junta vídeo e áudio fMP4 sem recodificar.
  - `hlsengine` descarrega **várias faixas** (retoma por faixa) para HLS com `#EXT-X-MEDIA` e para **DASH** (`SourceDASH`, `.mpd`, `linkrec.NewDASHAdapter`; MPD estático, 1 período, sem DRM; `SegmentTemplate`/`SegmentTimeline`/`SegmentList`/ficheiro único).
  - Captura e extensão passam a oferecer `.mpd`.
- **Dependência nova:** `core/go.mod` e `core/go.sum` foram alterados (só `mp4ff`; o `go.sum` ganhou 3 linhas). As linhas foram geradas aqui buscando o módulo diretamente ao GitHub (o proxy do Go está bloqueado neste ambiente) e provadas suficientes com `go build -mod=readonly`. **Confirme com rede:** `cd core && go mod tidy && go mod verify` (compara com o sumdb oficial; não deve alterar nada).
- Verificado aqui: `gofmt` e `go vet ./...` limpos; `go test -race` verde em todos os pacotes **exceto** os 5 testes SQLite de `orchestrator/integration_sqlite_test.go` e o pacote `storage` (só type-check, via `-modfile` alternativo com SQLite *stub*). `node --test extension/test/*.test.js`: 17/17. Os resultados juntados foram validados pelo `ffprobe`/`ffmpeg` (descodificam sem erros). Cada garantia de segurança/comportamento foi confirmada por mutação (uma mutação era equivalente e ficou documentada no ADR-011).
- **Teste intermitente já existente (não relacionado):** `httpengine` `TestSegmentedPauseResumeKeepsPerSegmentProgress` falhou ocasionalmente numa corrida completa e passou 30/30 isolado; hipótese **não confirmada**, nada alterado. Nas corridas completas desta sessão passou.

## 1. Primeiro, no novo chat
1. Subir para o GitHub: o workflow **CI** corre o SQLite real, o `go mod verify`/`tidy` e os testes em Linux e Windows (ver `docs/RELEASE.md`). Se algo falhar aí, é o primeiro sítio a olhar. Localmente: `cd core && gofmt -l . && go vet ./... && go test -race ./...` com rede.
2. `go mod tidy && go mod verify` (ver acima) e validar com o mundo real (secção 2) e só então ativar em `website/site-config.json` as funcionalidades que entraram `enabled: false`: `hls`, `login-downloads`, `scheduler`, `page-files` (e `torrent`).
3. Testar a extensão num Chromium real: opção de cookies (pedido de permissão), "Todos os ficheiros", "Agendar para". Os testes Node cobrem a lógica e os pedidos, não a UI do navegador.
4. Reproduzir ficheiros HLS (`.ts`/`.mp4`) num leitor real.

## 2. Validar no mundo real (nada disto foi possível aqui)
- **Cookies:** um site real com login (um ficheiro privado): descarregar com a opção ligada; confirmar que sem ela falha, e que reiniciar o StreamVault dá `auth_required`.
- **Agendador:** agendar para daqui a 2 minutos, fechar e reabrir o StreamVault antes e depois da hora.
- **Grabber:** uma página real de downloads (com e sem paginação `rel="next"`).
- **DASH / HLS com áudio separado:** um `.mpd` e um `.m3u8` master com `#EXT-X-MEDIA` reais e sem DRM: descarregar, abrir o `.mp4` resultante em VLC/Windows Media/Android e verificar **sincronia de áudio/vídeo** e a procura (seek). O ffmpeg decodifica sem erros nos fixtures, mas isso não substitui leitores reais.
- **HLS:** um `.m3u8` público em demanda sem cifra (ex.: vídeo de demonstração de um fornecedor de streaming que o permita): descarregar, pausar/retomar, comparar a duração/tamanho e reproduzir. Testar também um stream cifrado e um ao vivo para confirmar as recusas explicadas.
- **Torrent/magnet** com um magnet real (ex.: ISO Linux oficial): descarregar, verificar peças, retomar. Só depois pôr `torrent` a `enabled: true` no site.
- **Velocidade:** `svbench -url <ficheiro real> -connections 1,2,4,8,16` na vossa rede e comparar com o IDM no mesmo URL. Não afirmar ganhos sem isto.
- **Extensão:** instalar em modo de programador e testar em sites reais.
- Correr `go test ./...` com rede (SQLite).

## 3. Funcionalidades em falta (por ordem de impacto face ao IDM/1DM)
1. **HLS em MPEG-TS com áudio em separado** e **WebM em separado**: continuam recusados (exigiriam um muxer de TS/Matroska). Também por fazer: escolha de idioma/qualidade, legendas, DASH com vários períodos, `byte-ranges`.
2. **Torrent:** uTP, cifra de protocolo, PEX; DHT só como cliente.
3. **Extensão:** publicar na loja (Chrome/Edge); manifesto próprio para Firefox.
4. **LAN:** receber ficheiros exige confirmação na UI (a API/UI web e a fachada móvel ainda não a expõem).
5. **Agendador:** só "iniciar às X" (sem recorrência nem janelas horárias nem limite de velocidade por horário); a UI web local e a fachada móvel não o expõem (só API e extensão); `StartScheduler` não é chamado na fachada móvel.
6. **Grabber:** só HTML estático (sem JavaScript) e só `rel="next"`; a UI web local não o expõe.
7. **Cookies:** não sobrevivem a um reinício (por desenho); sem Authorization/Basic; sem login embutido.

## 4. Produção (precisa de contas/ferramentas externas)
- Certificado de assinatura de código; instalador Windows.
- **PoC gomobile / AAR Android** (critérios no ADR-003).
- Publicar o website; escolher o fornecedor de pagamentos (`checkout_url` está vazio de propósito).
- Chaves de licença: gerar offline com `cmd/licenseissuer` e embutir só a `.pub`.

## Notas úteis
- Dependências Go: `go.mod`/`go.sum` estão **inalterados** (só biblioteca padrão nos pacotes novos).
- No sandbox usei um `go.mod` alternativo com um módulo SQLite *stub* só para type-check; não faz parte do projeto.
- Defeitos reais já apanhados e protegidos por teste: segmento órfão quando um trabalhador se reforma (rondas), pasta de destino não criada, peer expulso a receber peças.
