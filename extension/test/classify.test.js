'use strict';
const test = require('node:test');
const assert = require('node:assert');
const C = require('../lib/classify.js');

const resp = (url, headers = {}, extra = {}) => ({
  url, statusCode: 200, method: 'GET',
  responseHeaders: Object.entries(headers).map(([name, value]) => ({ name, value })), ...extra,
});

test('deteta vídeo pelo Content-Type e pela extensão', () => {
  assert.deepStrictEqual(C.classifyResponse(resp('https://cdn.example.com/x', { 'Content-Type': 'video/mp4', 'Content-Length': '5000000' })),
    { url: 'https://cdn.example.com/x', kind: 'video', size: 5000000, supported: true });
  const byExt = C.classifyResponse(resp('https://cdn.example.com/aula.mkv?sig=1', { 'Content-Type': 'application/octet-stream' }));
  assert.strictEqual(byExt.kind, 'video');
  assert.strictEqual(C.classifyResponse(resp('https://cdn.example.com/s.mp3', { 'Content-Type': 'audio/mpeg', 'Content-Length': '4000000' })).kind, 'audio');
});

test('HLS e DASH são detetados e oferecidos', () => {
  const byMime = C.classifyResponse(resp('https://x.example.com/play', { 'Content-Type': 'application/vnd.apple.mpegurl' }));
  assert.deepStrictEqual(byMime, { url: 'https://x.example.com/play', kind: 'stream', size: -1, supported: true });
  const byExt = C.classifyResponse(resp('https://x.example.com/aula.m3u8?tok=1', { 'Content-Type': 'text/plain' }));
  assert.strictEqual(byExt.kind, 'stream'); assert.strictEqual(byExt.supported, true);
  const dashExt = C.classifyResponse(resp('https://x.example.com/a.mpd', {}));
  assert.strictEqual(dashExt.kind, 'stream'); assert.strictEqual(dashExt.supported, true);
  assert.strictEqual(dashExt.reason, undefined);
  const dashMime = C.classifyResponse(resp('https://x.example.com/manifest', { 'Content-Type': 'application/dash+xml' }));
  assert.strictEqual(dashMime.kind, 'stream'); assert.strictEqual(dashMime.supported, true);
});

test('respostas 206 usam o tamanho total do Content-Range', () => {
  const r = C.classifyResponse(resp('https://x.example.com/v.mp4', { 'Content-Type': 'video/mp4', 'Content-Range': 'bytes 0-1023/73400320', 'Content-Length': '1024' }, { statusCode: 206 }));
  assert.strictEqual(r.size, 73400320);
});

test('ignora anúncios/miniaturas pequenos, mas não quando o tamanho é desconhecido', () => {
  assert.strictEqual(C.classifyResponse(resp('https://x.example.com/ad.mp4', { 'Content-Type': 'video/mp4', 'Content-Length': '20000' })), null);
  assert.strictEqual(C.classifyResponse(resp('https://x.example.com/click.mp3', { 'Content-Type': 'audio/mpeg', 'Content-Length': '9000' })), null);
  assert.strictEqual(C.classifyResponse(resp('https://x.example.com/v.mp4', { 'Content-Type': 'video/mp4' })).size, -1);
});

test('ignora o que não é mídia ou não é descarregável', () => {
  for (const r of [
    resp('https://x.example.com/page.html', { 'Content-Type': 'text/html' }),
    resp('https://x.example.com/app.js', { 'Content-Type': 'application/javascript' }),
    resp('https://x.example.com/v.mp4', { 'Content-Type': 'video/mp4' }, { statusCode: 404 }),
    resp('https://x.example.com/v.mp4', { 'Content-Type': 'video/mp4' }, { method: 'POST' }),
    resp('blob:https://x.example.com/abc', { 'Content-Type': 'video/mp4' }),
    resp('ftp://x.example.com/v.mp4', {}),
    resp('chrome-extension://abc/v.mp4', { 'Content-Type': 'video/mp4' }),
    resp('https://x.example.com/seg.mp4?range=0-999999', { 'Content-Type': 'video/mp4' }),
    resp('https://x.example.com/seg.mp4?id=1&bytestart=100', { 'Content-Type': 'video/mp4' }),
    null, {},
  ]) assert.strictEqual(C.classifyResponse(r), null, JSON.stringify(r));
});

test('um parâmetro parecido (ex.: "ranges") não bloqueia a captura', () => {
  assert.ok(C.classifyResponse(resp('https://x.example.com/v.mp4?ranges=1', { 'Content-Type': 'video/mp4' })));
});

test('parseConnection: só loopback com chave hex válida', () => {
  const tk = 'a'.repeat(48);
  assert.deepStrictEqual(C.parseConnection(` http://127.0.0.1:47800/#token=${tk} `), { base: 'http://127.0.0.1:47800', token: tk });
  assert.deepStrictEqual(C.parseConnection(`http://localhost:5000/#token=${tk}`), { base: 'http://localhost:5000', token: tk });
  for (const bad of [
    `http://evil.example.com:47800/#token=${tk}`, `https://127.0.0.1:47800/#token=${tk}`, `http://192.168.1.5:47800/#token=${tk}`,
    'http://127.0.0.1:47800/', 'http://127.0.0.1:47800/#token=curto', `http://127.0.0.1:47800/#token=${'z'.repeat(48)}`, 'lixo', '', null,
  ]) assert.strictEqual(C.parseConnection(bad), null, String(bad));
});

test('mergeCandidates deduplica por URL e preserva a ordem', () => {
  const m = C.mergeCandidates([[{ url: 'a' }, { url: 'b' }], [{ url: 'b' }, { url: 'c' }], null, [{}]]);
  assert.deepStrictEqual(m.map(x => x.url), ['a', 'b', 'c']);
});

test('humanSize e displayName', () => {
  assert.strictEqual(C.humanSize(-1), '—');
  assert.strictEqual(C.humanSize(1536), '1.5 KB');
  assert.strictEqual(C.humanSize(5 * 1024 * 1024), '5.0 MB');
  assert.strictEqual(C.displayName('https://x.example.com/a/Aula%201.mp4?sig=1'), 'Aula 1.mp4');
  assert.strictEqual(C.displayName('https://x.example.com/'), 'x.example.com');
});

test('cookieHeader: ordena por especificidade, descarta inválidos e respeita o limite', () => {
  assert.strictEqual(C.cookieHeader([]), '');
  assert.strictEqual(C.cookieHeader(null), '');
  assert.strictEqual(C.cookieHeader([
    { name: 'a', value: '1', path: '/' }, { name: 'b', value: '2', path: '/x/y' },
    { name: 'nome inválido', value: 'z' }, { name: 'c', value: 'v;w' }, { name: 'd', value: 'ok\r\nX: 1' },
  ]), 'b=2; a=1');
  const big = [];
  for (let i = 0; i < 400; i++) big.push({ name: 'c' + String(i).padStart(3, '0'), value: 'x'.repeat(40), path: '/' });
  const h = C.cookieHeader(big);
  assert.ok(h.length > 0 && h.length <= 8192, 'tamanho ' + h.length);
  assert.ok(!/[;]\s*$/.test(h), 'não termina a meio de um cookie');
});
