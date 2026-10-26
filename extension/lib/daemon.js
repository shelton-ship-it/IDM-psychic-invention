// Cliente do daemon local. Só fala com 127.0.0.1 e envia sempre o token.
(function (root) {
  'use strict';

  function load() {
    return chrome.storage.local.get(['base', 'token']).then(function (v) {
      if (!v.base || !v.token) throw new Error('not-configured');
      return v;
    });
  }

  function call(method, path, body) {
    return load().then(function (c) {
      return fetch(c.base + path, {
        method: method,
        headers: { 'Authorization': 'Bearer ' + c.token, 'Content-Type': 'application/json' },
        body: body ? JSON.stringify(body) : undefined
      }).then(function (r) {
        if (r.status === 204) return {};
        return r.json().catch(function () { return {}; }).then(function (j) {
          if (!r.ok) { var e = new Error((j.error && j.error.message) || ('HTTP ' + r.status)); e.action = j.error && j.error.action; throw e; }
          return j;
        });
      });
    });
  }

  // Cookies do site (downloads atrás de login). Só se o utilizador ligou a opção E
  // concedeu a permissão "cookies"; só os cookies do próprio endereço pedido.
  // Qualquer falha devolve '' (o download segue sem credenciais).
  function cookieFor(url) {
    return chrome.storage.local.get('sendCookies').then(function (v) {
      if (!v.sendCookies) return '';
      return chrome.permissions.contains({ permissions: ['cookies'] }).then(function (ok) {
        if (!ok) return '';
        return chrome.cookies.getAll({ url: url }).then(function (list) { return root.SVClassify.cookieHeader(list); });
      });
    }).catch(function () { return ''; });
  }

  function withCookie(url, body) {
    return cookieFor(url).then(function (ck) { if (ck) body.cookie = ck; return body; });
  }

  root.SVDaemon = {
    health: function () { return call('GET', '/v1/health'); },
    capture: function (url) {
      return withCookie(url, { url: url, user_agent: navigator.userAgent }).then(function (b) { return call('POST', '/v1/capture', b); });
    },
    add: function (url, referer) {
      return withCookie(url, { url: url, referer: referer || '', user_agent: navigator.userAgent })
        .then(function (b) { return call('POST', '/v1/tasks', b); });
    },
    // todos os ficheiros descarregáveis da página (grabber); opts: categories, extensions, contains, max_pages
    links: function (pageUrl, opts) {
      var b = { url: pageUrl, user_agent: navigator.userAgent };
      Object.keys(opts || {}).forEach(function (k) { b[k] = opts[k]; });
      return withCookie(pageUrl, b).then(function (x) { return call('POST', '/v1/capture/links', x); });
    },
    // vários de uma vez; o cookie da página só segue para ligações do mesmo site (o núcleo garante)
    batch: function (urls, pageUrl) {
      return withCookie(pageUrl, { urls: urls, referer: pageUrl, user_agent: navigator.userAgent })
        .then(function (b) { if (b.cookie) b.cookie_url = pageUrl; return call('POST', '/v1/tasks/batch', b); });
    },
    // agenda o início para uma hora (ISO 8601)
    schedule: function (url, atISO, referer) {
      return withCookie(url, { url: url, at: atISO, referer: referer || '', user_agent: navigator.userAgent })
        .then(function (b) { return call('POST', '/v1/schedule', b); });
    },
    scheduled: function () { return call('GET', '/v1/schedule'); },
    unschedule: function (id) { return call('DELETE', '/v1/schedule/' + encodeURIComponent(id)); }
  };
})(typeof self !== 'undefined' ? self : this);
