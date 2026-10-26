// Lógica pura da extensão (sem APIs do navegador), testável em Node.
// Classifica respostas de rede como mídia descarregável, como o IDM.
(function (root) {
  'use strict';

  var VIDEO_EXT = ['mp4', 'webm', 'm4v', 'mov', 'mkv', 'avi', 'flv', 'wmv', '3gp', 'ogv'];
  var AUDIO_EXT = ['mp3', 'm4a', 'aac', 'ogg', 'oga', 'opus', 'wav', 'flac'];
  var STREAM_EXT = ['m3u8', 'mpd'];
  // abaixo disto (quando o tamanho é conhecido) costuma ser anúncio, miniatura ou som de UI
  var MIN_VIDEO = 100 * 1024;
  var MIN_AUDIO = 50 * 1024;

  function extOf(url) {
    try {
      var p = new URL(url).pathname;
      var m = /\.([a-z0-9]{2,5})$/i.exec(p);
      return m ? m[1].toLowerCase() : '';
    } catch (e) { return ''; }
  }

  function kindFromMime(ct) {
    ct = String(ct || '').split(';')[0].trim().toLowerCase();
    if (ct === 'application/vnd.apple.mpegurl' || ct === 'application/x-mpegurl' || ct === 'application/dash+xml') return 'stream';
    if (ct.indexOf('video/') === 0) return 'video';
    if (ct.indexOf('audio/') === 0) return 'audio';
    return '';
  }

  function kindFromExt(url) {
    var e = extOf(url);
    if (VIDEO_EXT.indexOf(e) >= 0) return 'video';
    if (AUDIO_EXT.indexOf(e) >= 0) return 'audio';
    if (STREAM_EXT.indexOf(e) >= 0) return 'stream';
    return '';
  }

  // tamanho total: Content-Length, ou o total de "Content-Range: bytes a-b/total" (respostas 206)
  function totalSize(headers) {
    var cr = headers['content-range'];
    if (cr) {
      var m = /\/(\d+)\s*$/.exec(cr);
      if (m) return parseInt(m[1], 10);
    }
    var cl = headers['content-length'];
    if (cl && /^\d+$/.test(cl)) return parseInt(cl, 10);
    return -1;
  }

  function headerMap(list) {
    var out = {};
    (list || []).forEach(function (h) { out[String(h.name).toLowerCase()] = String(h.value || ''); });
    return out;
  }

  // Devolve {url, kind, size, supported} ou null se não for mídia a oferecer.
  // HLS (.m3u8) e DASH (.mpd) são descarregados pelo núcleo (ADR-009/011); cifra/DRM e
  // ao vivo são recusados lá, com explicação.
  function classifyResponse(r) {
    if (!r || !/^https?:/i.test(r.url || '')) return null;
    if (r.statusCode && r.statusCode !== 200 && r.statusCode !== 206) return null;
    if (r.method && r.method !== 'GET') return null;
    // pedaços DASH/MSE (range=…) inundariam a lista sem serem ficheiros utilizáveis
    try {
      var keys = Array.from(new URL(r.url).searchParams.keys());
      for (var i = 0; i < keys.length; i++) if (/^(range|bytestart|byterange)$/i.test(keys[i])) return null;
    } catch (e) { return null; }
    var h = headerMap(r.responseHeaders);
    var kind = kindFromMime(h['content-type']) || kindFromExt(r.url);
    if (!kind) return null;
    // um "text/plain"/"application/octet-stream" com extensão de mídia continua válido pela extensão
    var size = totalSize(h);
    if (kind === 'video' && size >= 0 && size < MIN_VIDEO) return null;
    if (kind === 'audio' && size >= 0 && size < MIN_AUDIO) return null;
    return { url: r.url, kind: kind, size: size, supported: true };
  }

  function humanSize(n) {
    if (n == null || n < 0) return '—';
    var u = ['B', 'KB', 'MB', 'GB', 'TB'], i = 0;
    while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
    return (i ? n.toFixed(1) : String(n)) + ' ' + u[i];
  }

  // nome para mostrar: último segmento do caminho, sem query
  function displayName(url) {
    try {
      var p = new URL(url).pathname.split('/').filter(Boolean).pop() || new URL(url).host;
      return decodeURIComponent(p);
    } catch (e) { return url; }
  }

  // Aceita o endereço que o daemon mostra ("http://127.0.0.1:47800/#token=…") e
  // devolve {base, token}; recusa tudo o que não seja loopback.
  function parseConnection(text) {
    var u;
    try { u = new URL(String(text || '').trim()); } catch (e) { return null; }
    if (u.protocol !== 'http:') return null;
    var host = u.hostname.replace(/^\[|\]$/g, '');
    if (host !== '127.0.0.1' && host !== 'localhost' && host !== '::1') return null;
    var token = new URLSearchParams(u.hash.replace(/^#/, '')).get('token') || '';
    if (!/^[0-9a-f]{32,}$/i.test(token)) return null;
    return { base: u.origin, token: token };
  }

  // Junta capturas de rede e da página, sem repetir o mesmo URL.
  function mergeCandidates(lists) {
    var seen = {}, out = [];
    lists.forEach(function (l) {
      (l || []).forEach(function (c) {
        if (c && c.url && !seen[c.url]) { seen[c.url] = true; out.push(c); }
      });
    });
    return out;
  }

  // Monta o cabeçalho Cookie ("a=1; b=2") a partir do resultado de chrome.cookies.getAll.
  // Mais específicos primeiro (caminho mais longo), como os navegadores. Ignora
  // nomes/valores inválidos (caracteres de controlo, ";") e respeita o limite do
  // núcleo (8192): os menos específicos são os que ficam de fora.
  var MAX_COOKIE_HEADER = 8192;
  var TOKEN = /^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$/;
  function cookieHeader(list) {
    var ok = (list || []).filter(function (c) {
      return c && typeof c.name === 'string' && typeof c.value === 'string' &&
        TOKEN.test(c.name) && /^[\x20-\x7e]*$/.test(c.value) && c.value.indexOf(';') < 0;
    });
    ok.sort(function (a, b) {
      return (String(b.path || '/').length - String(a.path || '/').length) || (a.name < b.name ? -1 : a.name > b.name ? 1 : 0);
    });
    var out = '';
    for (var i = 0; i < ok.length; i++) {
      var piece = ok[i].name + '=' + ok[i].value;
      var next = out ? out + '; ' + piece : piece;
      if (next.length > MAX_COOKIE_HEADER) break;
      out = next;
    }
    return out;
  }

  var api = { cookieHeader: cookieHeader, classifyResponse: classifyResponse, humanSize: humanSize, displayName: displayName,
    parseConnection: parseConnection, mergeCandidates: mergeCandidates, kindFromMime: kindFromMime, kindFromExt: kindFromExt };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else root.SVClassify = api;
})(typeof self !== 'undefined' ? self : this);
