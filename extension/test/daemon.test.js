'use strict';
const test = require('node:test');
const assert = require('node:assert');

// ambiente mínimo de extensão: chrome.storage/permissions/cookies e fetch simulados
function setup({ sendCookies = false, granted = false, cookies = [] } = {}) {
  const calls = [];
  global.self = global;
  Object.defineProperty(global, 'navigator', { value: { userAgent: 'UA-teste' }, configurable: true, writable: true });
  global.SVClassify = require('../lib/classify.js');
  global.chrome = {
    storage: { local: { get: async (k) => {
      const all = { base: 'http://127.0.0.1:47800', token: 'tok', sendCookies };
      if (Array.isArray(k)) return Object.fromEntries(k.map((x) => [x, all[x]]));
      return { [k]: all[k] };
    } } },
    permissions: { contains: async () => granted },
    cookies: { getAll: async (q) => { calls.push({ cookiesFor: q.url }); return cookies; } },
  };
  global.fetch = async (url, init) => {
    calls.push({ url, method: init.method, body: init.body ? JSON.parse(init.body) : undefined, auth: init.headers.Authorization });
    return { ok: true, status: init.method === 'DELETE' ? 204 : 200, json: async () => ({ ok: true }) };
  };
  delete require.cache[require.resolve('../lib/daemon.js')];
  require('../lib/daemon.js');
  return { D: global.SVDaemon, calls };
}
const sent = (calls) => calls.filter((c) => c.url);

const jar = [
  { name: 'sid', value: 'abc', path: '/' },
  { name: 'pref', value: 'x', path: '/app/area' },
  { name: 'mau', value: 'a;b', path: '/' },
];

test('por defeito NÃO lê nem envia cookies', async () => {
  const { D, calls } = setup({ cookies: jar });
  await D.add('https://site.example.com/f.zip', 'https://site.example.com/pag');
  assert.strictEqual(calls.filter((c) => c.cookiesFor).length, 0, 'não devia sequer consultar os cookies');
  const b = sent(calls)[0].body;
  assert.strictEqual(b.cookie, undefined);
  assert.strictEqual(b.url, 'https://site.example.com/f.zip');
  assert.strictEqual(sent(calls)[0].auth, 'Bearer tok');
});

test('permissão concedida mas opção DESLIGADA: não lê nem envia cookies', async () => {
  const { D, calls } = setup({ sendCookies: false, granted: true, cookies: jar });
  await D.add('https://site.example.com/f.zip');
  assert.strictEqual(calls.filter((c) => c.cookiesFor).length, 0, 'a opção do utilizador manda, mesmo com a permissão dada');
  assert.strictEqual(sent(calls)[0].body.cookie, undefined);
});

test('opção ligada mas permissão em falta: continua sem cookies', async () => {
  const { D, calls } = setup({ sendCookies: true, granted: false, cookies: jar });
  await D.add('https://site.example.com/f.zip');
  assert.strictEqual(calls.filter((c) => c.cookiesFor).length, 0);
  assert.strictEqual(sent(calls)[0].body.cookie, undefined);
});

test('opção + permissão: envia só os cookies do endereço pedido, válidos e ordenados', async () => {
  const { D, calls } = setup({ sendCookies: true, granted: true, cookies: jar });
  await D.add('https://site.example.com/f.zip', 'https://site.example.com/pag');
  assert.deepStrictEqual(calls.find((c) => c.cookiesFor), { cookiesFor: 'https://site.example.com/f.zip' });
  assert.strictEqual(sent(calls)[0].body.cookie, 'pref=x; sid=abc'); // "mau" (com ;) descartado
});

test('lote: cookie da página + cookie_url; sem cookie não há cookie_url', async () => {
  let s = setup({ sendCookies: true, granted: true, cookies: jar });
  await s.D.batch(['https://site.example.com/a.pdf', 'https://cdn.example.com/b.zip'], 'https://site.example.com/lista');
  let b = sent(s.calls)[0];
  assert.strictEqual(b.url.endsWith('/v1/tasks/batch'), true);
  assert.strictEqual(b.body.cookie_url, 'https://site.example.com/lista');
  assert.strictEqual(b.body.cookie, 'pref=x; sid=abc');
  assert.strictEqual(b.body.urls.length, 2);

  s = setup({ sendCookies: false, cookies: jar });
  await s.D.batch(['https://site.example.com/a.pdf'], 'https://site.example.com/lista');
  b = sent(s.calls)[0].body;
  assert.strictEqual(b.cookie, undefined);
  assert.strictEqual(b.cookie_url, undefined);
});

test('links, agendar, listar e remover usam as rotas certas', async () => {
  const { D, calls } = setup();
  await D.links('https://site.example.com/lista', { max_pages: 3, categories: ['Documentos'] });
  await D.schedule('https://site.example.com/f.zip', '2026-10-25T02:00:00.000Z', 'https://site.example.com/');
  await D.scheduled();
  await D.unschedule('a/b c');
  const s = sent(calls);
  assert.ok(s[0].url.endsWith('/v1/capture/links') && s[0].body.max_pages === 3 && s[0].body.categories[0] === 'Documentos');
  assert.ok(s[1].url.endsWith('/v1/schedule') && s[1].body.at === '2026-10-25T02:00:00.000Z' && s[1].method === 'POST');
  assert.ok(s[2].url.endsWith('/v1/schedule') && s[2].method === 'GET');
  assert.ok(s[3].url.endsWith('/v1/schedule/a%2Fb%20c') && s[3].method === 'DELETE'); // id escapado
});

test('falha ao ler cookies não impede o download (segue sem credenciais)', async () => {
  const { D, calls } = setup({ sendCookies: true, granted: true });
  global.chrome.cookies.getAll = async () => { throw new Error('boom'); };
  await D.add('https://site.example.com/f.zip');
  assert.strictEqual(sent(calls)[0].body.cookie, undefined);
});
