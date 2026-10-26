# ADR-005 — Website e produção (F8/F12)

- **Website:** página única estática (`website/`), sem dependências externas. Preço, textos e ligações vêm de `site-config.json`. Funcionalidades com `"enabled": false` não aparecem, por isso a página nunca promete o que o produto ainda não entrega (hoje: Magnet/.torrent).
- **Compra:** o fornecedor de pagamentos não está definido. `checkout_url` vazio mostra "A compra ainda não está aberta". Não inventei nenhum fornecedor.
- **Build:** `scripts/build-windows.sh` produz um `.exe` estático (`CGO_ENABLED=0`, `-trimpath`) com versão/commit e `SHA256SUMS`. Verificado: duas compilações dão o mesmo SHA-256.
- **Emissor de licenças:** compila para `internal-only/` e nunca entra no pacote do cliente.
- **CI:** gofmt, vet e testes (com `-race` no Linux) em Linux e Windows, mais compilação do daemon Windows.
- **Pendente, por exigir contas/ferramentas externas:** certificado de assinatura de código, instalador, publicação do website e escolha do fornecedor de pagamentos (ver `docs/RELEASE.md`).
