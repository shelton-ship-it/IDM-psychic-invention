# Extensão do StreamVault (Chrome/Edge, Manifest V3)

Deteta vídeo e áudio nas páginas (como o IDM) e envia-os ao StreamVault, que corre no seu computador. Só fala com `127.0.0.1`.

## Instalar (desenvolvimento)
1. Inicie o StreamVault (`streamvault`). Mostra uma linha `Abra: http://127.0.0.1:47800/#token=…`.
2. No Chrome/Edge: `chrome://extensions` → ativar *Modo de programador* → *Carregar sem compactar* → escolher esta pasta.
3. Abra as *Definições* da extensão e cole essa linha completa. A chave persiste entre arranques (`api-token` na pasta de dados); `streamvault -new-token` gera outra.

## Como funciona
- `background.js` observa as respostas de rede (`webRequest`) e guarda, por separador, as mídias detetadas (vídeo/áudio por `Content-Type` ou extensão). Ignora respostas minúsculas (anúncios) e pedaços `range=`.
- O botão **Descarregar** envia o URL com o `Referer` da página e o `User-Agent` do navegador: muitos servidores recusam sem eles.
- **Procurar vídeo** pede ao StreamVault para analisar o HTML da página (`/v1/capture`).
- **Todos os ficheiros** lista as ligações para ficheiros descarregáveis da página (PDF, ZIP, programas…; `/v1/capture/links`), para escolher e descarregar vários de uma vez (`/v1/tasks/batch`).
- **Agendar para** (campo de data/hora no popup): em vez de descarregar já, agenda o início (`/v1/schedule`).
- **Cookies (opcional, desligado por defeito):** em Definições, “Enviar os cookies do site ao StreamVault” pede a permissão `cookies` ao navegador. Só vão os cookies do endereço que está a descarregar, e ficam só na memória do StreamVault (ADR-010).

## Permissões (e porquê)
- `webRequest` + `<all_urls>`: ver os cabeçalhos das respostas para detetar mídia. A extensão **não lê o conteúdo** das páginas nem altera pedidos.
- `tabs`: saber o endereço e o título do separador ativo.
- `storage`: guardar o endereço do StreamVault e a lista de mídias por separador.

## O que não faz
HLS (`.m3u8`) e DASH (`.mpd`) são oferecidos para descarregar (o núcleo só aceita streams sem cifra/DRM e em demanda; cifrado, ao vivo ou em formatos não suportados falha com explicação; vídeo e áudio em separado são juntados em MP4); não contorna DRM nem logins. Os cookies só são enviados se o utilizador ligar a opção em Definições (ver abaixo).

## Testes
`npm test` (ou `node --test test/*.test.js`) corre a lógica de classificação. O ciclo completo foi verificado num Chromium real: ligação ao daemon, captura por rede, popup e download com `Referer`.
