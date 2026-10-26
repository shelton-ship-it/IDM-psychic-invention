# ADR-004 — Handoff entre dispositivos na LAN (F10)

- **Estado:** implementado em `core/lan` e testado em loopback.
- **Ameaça:** a LAN é hostil. Qualquer dispositivo pode ligar-se ao recetor.
- **Protocolo:** descoberta por UDP (anúncio é só uma pista, nada nele é de confiança) → TCP com X25519 efémero → PIN de 6 dígitos mostrado **no ecrã do recetor** (uso único, 2 min, 3 tentativas) → canal AES-256-GCM com contador por direção.
- **Ligação do PIN às chaves:** cada lado envia um HMAC (chave derivada do PIN) sobre o transcript e as chaves de sessão. Consequências:
  - um intermediário ativo só pode adivinhar o PIN online, 3 vezes por janela;
  - um observador passivo não consegue testar PINs offline, porque o MAC depende do segredo Diffie-Hellman;
  - o recetor autentica o cliente por si (testado com um cliente que ignora a prova do servidor, e com mutação do código).
- **Recetor:** nomes só simples e validados (`security.CheckFileName`), tamanho máximo, SHA-256 obrigatório, escrita em ficheiro temporário exclusivo, nunca sobrescreve, nunca executa. Um ficheiro só é aceite com confirmação explícita do utilizador (`OnFile`). Links entram por `Core.Add` e passam por toda a política do núcleo.
- **Licença:** exige `LANTransfer`; o daemon só abre o pareamento com licença válida que o inclua.
- **Decisões de âmbito:**
  - o PIN é pedido em cada sessão (sem dispositivos "de confiança" persistentes; evita guardar chaves de longo prazo);
  - o daemon do Windows aceita só links por LAN; ficheiros ficam para UIs que implementem a confirmação (a fachada móvel e a UI web precisam de a expor).
- **Fora de âmbito:** retoma de ficheiros interrompidos, múltiplos recetores em simultâneo e travessia de NAT.
