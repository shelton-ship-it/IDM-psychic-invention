# ADR-003 — Arquitetura Windows e Android; gate Go-vs-Kotlin (F6/F7)

- **Estado:** decidido; o gate do Android depende de um PoC que ainda **não foi executado**.
- **Decisão F6 (Windows):** o núcleo corre como daemon local (`cmd/streamvault`) que serve uma API JSON + SSE e uma UI web embebida em `127.0.0.1`. A UI é só um cliente; nenhuma regra de downloads vive nela.
  - Segurança: só loopback, token Bearer por execução (vai no fragmento `#token=`, que o navegador nunca envia), validação de `Host` (DNS rebinding) e de `Origin` (CSRF), corpos limitados, JSON estrito, URL sempre redigida, CSP restrita e UI sem `innerHTML`.
  - O cliente nunca escolhe pastas: destino e biblioteca vêm da configuração.
  - Alternativas recusadas: UI nativa Win32/WinUI (duplica a lógica de ecrã que o Android também precisa) e Electron (runtime pesado sem ganho para este caso).
- **Decisão F7 (Android):** o núcleo Go compila para AAR com `gomobile bind` (`core/mobile`); Kotlin só faz UI, serviço em primeiro plano e acesso SAF. As duas UIs partilham os mesmos JSON (`core/view`). BitTorrent usa o **mesmo motor e a mesma verificação por peças** do Windows, em vez de uma segunda implementação em Kotlin.
- **Gate (a medir no PoC, num dispositivo de gama média):** se qualquer critério falhar, o BitTorrent no Android passa a Kotlin atrás da mesma interface `torrentengine.Backend`; HTTP, orquestrador, segurança e organização continuam em Go.
  1. AAR arm64-v8a ≤ 25 MB.
  2. `mobile.Open` ≤ 1 s a frio.
  3. Memória residente ≤ 150 MB com 1 torrent ativo.
  4. Sobrevive a pausa/retoma do serviço sem perder o estado.
  Estes limites são escolhas minhas para arrancar o PoC; ajuste-os se o alvo for outro.
- **Consequência:** `cmd/streamvault` e `core/mobile` compilam e estão testados aqui, mas o AAR e o empacotamento Windows precisam das ferramentas nativas (gomobile/NDK, instalador) fora deste ambiente.
