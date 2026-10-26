# StreamVault

Gestor de transferências que só dá um ficheiro como concluído depois de o verificar.

```
core/      núcleo em Go (motores HTTP, HLS/DASH e BitTorrent, cookies em memória, agendador, orquestrador, segurança, licenças, LAN, API local)
extension/ extensão de navegador (captura de vídeo, Manifest V3)
website/   página do produto (estática)
docs/      ADRs, estado das fases e guia de lançamento
scripts/   build reprodutível do Windows
```

- Estado das fases: `docs/FASES.md`
- Decisões de arquitetura: `docs/adr/`
- Lançamento: `docs/RELEASE.md`

Desenvolvimento: `make test` · `make vet` · `make build-windows VERSION=1.0.0` · `make package VERSION=1.0.0` (todos os sistemas + extensão + SHA256SUMS)
Executar localmente: `cd core && go run ./cmd/streamvault -open`
