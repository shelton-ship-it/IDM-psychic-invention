# ADR-010 — Cookies (downloads atrás de login), agendador e grabber de links

- **Estado:** implementado e testado localmente (servidores `httptest`, `go test -race`, testes Node da extensão). **Não** validado contra sites reais com login, nem a extensão num navegador real nesta versão. SQLite real e `storage` não correram neste ambiente (ver `docs/PENDENTE.md`).
- **Contexto:** faltavam três coisas face ao IDM/1DM: descarregar o que exige sessão iniciada, começar um download a uma hora marcada, e listar/descarregar de uma vez os ficheiros de uma página.

## 1. Cookies

### Decisão: credenciais só em memória
O `Metadata` da origem é persistido (SQLite), por isso um cookie lá ficaria em claro no disco. Optou-se por **nunca persistir credenciais**, em vez de depender de um cofre diferente por sistema (DPAPI no Windows, Keystore no Android) que não se consegue validar aqui.
- `app.Hints.Cookie` → `AddWith` valida o cookie, guarda-o num armazém **em memória** (`secretStore`) e persiste na origem apenas `cookie_ref` (token aleatório de 16 bytes) e `cookie_host` (não são segredos).
- Um decorador de motor (`cookieEngine`, registado para HTTP e HLS) copia a tarefa no `Start`, resolve o token e injeta o `Cookie` **na cópia em memória**. A tarefa persistida nunca o contém.
- Concluída a tarefa, o valor é apagado; `Core.Close` apaga tudo.
- **Depois de reiniciar** o valor já não existe: o `Start` falha com `auth_required` e uma ação clara ("inicie sessão e volte a adicionar a partir do navegador"), sem contactar o servidor. Nunca corre "sem login" para depois gravar uma página de erro como se fosse o ficheiro.
- Nova classe de erro `ErrAuthRequired` (acrescentada no fim, não altera os valores já persistidos).

### Âmbito: só o host exato
- `ApplyBrowserHints` só envia o `Cookie` se o host do pedido for **igual** a `cookie_host` (inclui a porta). Nunca vai a segmentos HLS de um CDN nem a outro host.
- `CheckRedirect` retira o `Cookie` num redirecionamento para outro host ou de https para http. Isto é necessário: o `net/http` só retira cabeçalhos sensíveis entre *domínios* distintos e ignora a porta (demonstrado pelo teste de vazamento).
- Mesma regra na captura de páginas (`capture.Options.Cookie`).
- Validação: no máximo 8192 bytes, só ASCII imprimível (sem `\r\n`: nada de injeção de cabeçalhos); cookies só para http(s)/HLS (magnet/torrent: recusado).
- A API nunca devolve o cookie; as vistas usam `Redacted()`; o lote devolve URLs sem query.

### API e extensão
- `POST /v1/tasks`, `/v1/capture`, `/v1/capture/links`, `/v1/schedule` aceitam `cookie`.
- A extensão pede `cookies` como **permissão opcional**, só quando o utilizador liga a opção em Definições (desligada por defeito). Envia só os cookies do endereço pedido (`chrome.cookies.getAll({url})`). Se a opção estiver desligada, nem sequer consulta os cookies, mesmo que a permissão tenha sido dada antes.

### Limites
- Perde-se ao reiniciar (por desenho). Uma transferência longa interrompida por um reinício tem de ser adicionada de novo.
- Cookies de um site cujo download redireciona para outro domínio **não** acompanham o redirecionamento (mais estrito do que um navegador). Alguns sites que servem o ficheiro de um domínio de CDN com sessão própria podem exigir tokens no URL em vez de cookies.
- Não há login embutido nem renovação de sessão. Cabeçalhos de autorização (Basic/Bearer) não são suportados.

## 2. Agendador (`core/scheduler`)
- Agenda o **início** de um download para uma hora (RFC 3339, até 1 ano à frente, máx. 1000 entradas). À hora marcada chama `Core.AddWith`, por isso herda segurança, fila e retoma.
- `Core.Schedule` valida **ao agendar** (reconhecimento, política de segurança, cookie), para o utilizador saber já se o link seria recusado.
- Persistência em `schedule.json` ao lado da base de dados (escrita atómica, 0600). Um ficheiro ilegível é erro (o daemon avisa e arranca sem agendador; nunca descarta a agenda).
- Entradas vencidas (por exemplo, o computador estava desligado) correm de imediato ao arrancar.
- Uma entrada que arranca sai da agenda (passa a tarefa normal). Uma que falha fica, com o motivo, e não é repetida em ciclo; o utilizador remove-a.
- Cookies: só em memória; uma entrada que os precisava e sobreviveu a um reinício falha com explicação (igual ao ponto 1).
- API: `POST/GET /v1/schedule`, `DELETE /v1/schedule/{id}`. A vista tem o URL sem query e nunca credenciais.
- **Limites:** só "iniciar às X" (sem recorrência, sem janelas "só entre as 02:00 e as 06:00", sem limite de velocidade por horário). A fachada móvel e a UI web local ainda não expõem o agendador (só a API e a extensão).

## 3. Grabber de links e lote
- `capture.FindLinks` / `POST /v1/capture/links`: lista as ligações para ficheiros descarregáveis de uma página (categorias do `organizer`: Documentos, Arquivos, Programas, Vídeos…). Filtros: `categories`, `extensions` extra, `contains`. `<img>` só se a categoria "Imagens" for pedida (senão inundaria a lista com ícones).
- Paginação: segue `rel="next"` até `max_pages` (máx. 10), **só no mesmo host** da primeira página, sem repetir páginas (proteção contra ciclos). Máx. 2000 ligações; assinala `truncated`.
- A mesma política de segurança e de credenciais de `Find` (código partilhado em `Options.get`).
- `POST /v1/tasks/batch` (até 200 ligações): resultado individual por ligação (uma inválida não pára o lote). O cookie só é aplicado às ligações cujo host é igual ao de `cookie_url`; as outras entram sem credenciais.
- **Limites:** só HTML estático (sem JavaScript: listas geradas no navegador não aparecem); paginação só por `rel="next"` (não adivinha `?page=2`); não percorre sub-páginas nem o site inteiro.

## Verificação
- Cada garantia de segurança foi confirmada por mutação (reverter o código faz um teste falhar): âmbito do host (HTTP e HLS), cookie não persistido, decorador registado, retirada no redirecionamento, paginação só no mesmo host, âmbito do cookie no lote, opt-in da extensão.
- Uma das mutações revelou um teste da extensão que não distinguia "opção desligada" de "permissão em falta"; foi corrigido com um caso próprio.
- `go vet` e `go test -race` limpos exceto os 5 testes SQLite do `orchestrator`, que dependem do módulo real. `node --test`: 17/17.
