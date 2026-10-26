# ADR-006 — Links abertos por defeito (paridade com IDM/1DM)

- **Estado:** decidido; **substitui** o defeito "redes privadas bloqueadas" do `security.go` original e da ADR-002 (camada de segurança).
- **Contexto:** o produto compete com IDM e 1DM, que descarregam de qualquer URL que o utilizador cola, incluindo servidores locais, NAS e `localhost`. Bloquear isto por defeito quebrava casos de uso comuns.
- **Análise de ameaça:** num download manager de desktop, o utilizador que cola um URL já é quem decide o destino. O ataque real é outro: um site **público** devolver um redirecionamento para `127.0.0.1`, o router ou um serviço interno.
- **Decisão:**
  1. `allow_private_networks` passa a `true` por defeito: URLs colados pelo utilizador funcionam, incluindo rede local e `localhost`.
  2. Redirecionamentos: **origem pública → destino privado continua bloqueado.** Origem privada → privada e qualquer → pública são permitidos (`httpengine.checkRedirect`).
  3. Adaptadores **compilados pelo próprio produto** (`plugin.RegisterBuiltin`) funcionam sem aprovação e são revogáveis pelo utilizador (a revogação persiste). Só adaptadores de terceiros exigem aprovação explícita (ADR-002).
  4. Modo estrito continua disponível: `allow_private_networks=false`.
- **Fora desta decisão, e que não foram alterados:** HLS/DRM protegido e conteúdo atrás de login alheio continuam fora de âmbito (ADR-002, princípios 11 e 26 do briefing). Magnet/.torrent dependem do adaptador anacrolix (F3), que ainda não existe.
- **Testes que fixam isto:** `TestDefaultConfigDownloadsFromLocalServer`, `TestLocalServerRedirectDownloads`, `TestRedirectToLoopbackBlocked` (origem pública), `TestRedirectPolicyPublicToPrivateBlockedPrivateToPrivateAllowed` e `TestBuiltinAdapterWorksByDefaultAndIsRevocable`. Verifiquei por mutação que os dois testes de redirecionamento falham se a regra for removida.
