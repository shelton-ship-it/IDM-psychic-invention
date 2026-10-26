# Guia de lançamento

## Antes de cada versão
1. `make test` e `make vet` limpos; CI verde em Linux e Windows.
2. Corra `go test ./...` **com rede** na sua máquina: `storage` e `orchestrator/integration_sqlite_test.go` só foram verificados a nível de tipos fora dela.
3. Reveja `website/site-config.json`: só ative funcionalidades que a versão entrega.
4. `scripts/build-windows.sh <versão>` e guarde `SHA256SUMS`.

## Compilar e empacotar no GitHub (Actions)
1. Faça commit de tudo (incluindo `.github/workflows/`, `scripts/package.sh`, `core/go.mod` e `core/go.sum`) e envie para o GitHub.
2. O workflow **CI** corre sozinho em cada *push* para `main` e em cada *pull request*: verifica as dependências (`go mod verify` e `go mod tidy` sem diferenças), `gofmt`, `go vet`, os testes (Linux com `-race` e ffmpeg; Windows) e a extensão. **É aqui que, pela primeira vez, corre o SQLite real.** Se o passo `go.mod/go.sum desatualizados` falhar, corra `go mod tidy` em `core/` e faça commit.
3. Para **publicar uma versão**: `git tag v1.0.0 && git push origin v1.0.0`. O workflow **Release** corre o CI, compila e empacota e cria uma **release em rascunho** com: o `.zip` do Windows, os `.tar.gz` de Linux e macOS, o `.zip` da extensão e o `SHA256SUMS`. Reveja e carregue em *Publish*. Etiquetas com sufixo (`v1.0.0-rc1`) ficam marcadas como pré-lançamento.
4. Sem etiqueta: *Actions → Release → Run workflow* gera os mesmos ficheiros como **artefacto** da execução (30 dias), sem criar release. Bom para testar.
5. Na sua máquina: `make package VERSION=1.0.0` (usa `scripts/package.sh`; precisa de `bash`, `zip`, `tar`, `python3`) gera o mesmo em `dist/`.

Os pacotes são reprodutíveis: os mesmos fontes dão os mesmos SHA-256 (verificado duas vezes seguidas). **Não estão assinados**: ver abaixo.

## Chaves de licença
- Gere o par com `licenseissuer keygen` numa máquina **offline**. A `.key` nunca entra no repositório nem no instalador.
- Embeba só a `.pub` e o `key_id` no cliente (hoje o daemon lê-os de `-license-pub` / `-license-key-id`; para produção, embuta-os com `go:embed`).
- Rotação: emita novas licenças com um `key_id` novo e mantenha as chaves públicas antigas no `Keyring` enquanto houver licenças válidas.

## Medir antes de afirmar velocidade
Antes de qualquer afirmação de desempenho (site, loja, comparação com o IDM/1DM): corra `svbench -url <um ficheiro real> -connections 1,2,4,8,16` na rede e com os servidores que os seus utilizadores usam, e compare com o IDM no **mesmo** URL e rede. Guarde os números. O ganho depende de o servidor limitar por ligação ou por IP.

## Ainda por fazer (precisa de contas ou ferramentas externas)
- **Assinatura de código** (Authenticode) do `.exe` e do instalador.
- **Instalador Windows** (por exemplo Inno Setup) que instale o daemon e abra `streamvault -open`.
- **Validar o torrent com um magnet real** antes de ativar `torrent` em `site-config.json`: arranque o daemon, adicione o magnet de uma distribuição Linux (por exemplo uma ISO oficial), confirme que descarrega, que a verificação por peças conclui e que retoma depois de fechar e reabrir. Os testes automáticos usam só enxames locais.
- **Extensão do navegador:** empacotar e publicar na loja do Chrome/Edge (precisa de conta de programador). Até lá instala-se em modo de programador (`extension/README.md`). Para o Firefox é preciso um manifesto próprio.
- **PoC gomobile** do ADR-003.
- **Publicação do website** e `checkout_url`.
- **Política de atualização:** hoje não existe canal de atualização automática; não afirme que existe.
