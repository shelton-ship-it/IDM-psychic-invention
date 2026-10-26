(function () {
  'use strict';
  var C = self.SVClassify, D = self.SVDaemon;
  var $ = function (id) { return document.getElementById(id); };
  var tab = null, captured = [], found = [], grabbed = [];

  function say(t, bad) { var s = $('st'); s.textContent = t || ''; s.className = bad ? 'bad' : ''; }
  function el(tag, cls, text) { var e = document.createElement(tag); if (cls) e.className = cls; if (text != null) e.textContent = text; return e; }

  function render() {
    var ul = $('list'); ul.textContent = '';
    var all = C.mergeCandidates([captured, found]);
    if (!all.length) { say('Nenhuma mídia detetada neste separador. Reproduza o vídeo ou use “Procurar na página”.'); return; }
    say('');
    all.forEach(function (c) {
      var li = el('li'), left = el('div', 'n');
      var t = el('div'); t.appendChild(el('span', 'k', c.kind)); t.appendChild(el('span', null, c.title || C.displayName(c.url)));
      left.appendChild(t);
      left.appendChild(el('div', 'm', C.displayName(c.url) + (c.size >= 0 ? ' · ' + C.humanSize(c.size) : '')));
      if (c.supported === false) left.appendChild(el('div', 'm', c.reason || 'Ainda não suportado.'));
      li.appendChild(left);
      if (c.supported !== false) {
        var b = el('button', 'btn', 'Descarregar');
        b.onclick = function () {
          b.disabled = true;
          startOne(c.url).then(function (when) { b.textContent = when ? 'Agendado' : 'Adicionado'; })
            .catch(function (e) { b.disabled = false; say(e.message + (e.action ? ' ' + e.action : ''), true); });
        };
        li.appendChild(b);
      }
      ul.appendChild(li);
    });
  }

  // hora escolhida no campo (ISO 8601 em UTC) ou '' se não houver agendamento
  function chosenTime() {
    var v = $('at').value;
    if (!v) return '';
    var d = new Date(v);
    return isNaN(d.getTime()) ? '' : d.toISOString();
  }

  // adiciona já, ou agenda se houver hora; resolve com a hora usada ('' = já)
  function startOne(url) {
    var at = chosenTime();
    if ($('at').value && !at) return Promise.reject(new Error('Hora inválida.'));
    return (at ? D.schedule(url, at, tab.url) : D.add(url, tab.url)).then(function () { return at; });
  }

  function selected() {
    return Array.prototype.map.call(document.querySelectorAll('#list input[type=checkbox]:checked'), function (i) { return i.value; });
  }
  function updateCount() { $('cnt').textContent = selected().length + ' selecionado(s)'; }

  // ficheiros descarregáveis da página (grabber), com seleção
  function renderLinks() {
    var ul = $('list'); ul.textContent = '';
    $('bar').hidden = !grabbed.length;
    if (!grabbed.length) { say('Esta página não tem ligações para ficheiros descarregáveis.'); return; }
    say(grabbed.length + ' ficheiro(s) encontrado(s).');
    grabbed.forEach(function (l) {
      var li = el('li'), left = el('div', 'n');
      var cb = document.createElement('input'); cb.type = 'checkbox'; cb.value = l.url; cb.onchange = updateCount;
      var t = el('div'); t.appendChild(cb); t.appendChild(el('span', null, ' ' + (l.text || C.displayName(l.url)))); t.appendChild(el('span', 'cat', l.category));
      left.appendChild(t);
      left.appendChild(el('div', 'm', C.displayName(l.url)));
      li.appendChild(left); ul.appendChild(li);
    });
    updateCount();
  }

  $('grab').onclick = function () {
    if (!tab || !/^https?:/.test(tab.url || '')) { say('Abra uma página http(s).', true); return; }
    say('A listar os ficheiros da página…');
    D.links(tab.url, { max_pages: 1 }).then(function (r) { grabbed = r.links || []; renderLinks(); }).catch(connectionHint);
  };

  $('dlsel').onclick = function () {
    var urls = selected();
    if (!urls.length) { say('Selecione pelo menos um ficheiro.', true); return; }
    var at = chosenTime();
    if ($('at').value && !at) { say('Hora inválida.', true); return; }
    $('dlsel').disabled = true;
    var run = at
      ? urls.reduce(function (p, u) { return p.then(function (acc) { return D.schedule(u, at, tab.url).then(function () { acc.ok++; return acc; }, function (e) { acc.fail++; acc.err = acc.err || e.message; return acc; }); }); }, Promise.resolve({ ok: 0, fail: 0, err: '' }))
      : D.batch(urls, tab.url).then(function (r) {
          var acc = { ok: 0, fail: 0, err: '' };
          (r.results || []).forEach(function (x) { if (x.ok) acc.ok++; else { acc.fail++; acc.err = acc.err || x.error; } });
          return acc;
        });
    run.then(function (acc) {
      $('dlsel').disabled = false;
      say((at ? 'Agendados: ' : 'Adicionados: ') + acc.ok + (acc.fail ? ' · falharam ' + acc.fail + (acc.err ? ' (' + acc.err + ')' : '') : ''), acc.fail > 0);
    }).catch(function (e) { $('dlsel').disabled = false; connectionHint(e); });
  };

  function connectionHint(e) {
    if (e && e.message === 'not-configured') say('Ligue a extensão ao StreamVault em “Definições”.', true);
    else say('Não foi possível falar com o StreamVault. Está a correr?', true);
  }

  $('opts').onclick = function () { chrome.runtime.openOptionsPage(); };
  $('find').onclick = function () {
    if (!tab || !/^https?:/.test(tab.url || '')) { say('Abra uma página http(s).', true); return; }
    say('A procurar…');
    D.capture(tab.url).then(function (r) {
      found = (r.candidates || []).map(function (x) { x.size = -1; return x; });
      render();
    }).catch(connectionHint);
  };

  chrome.tabs.query({ active: true, currentWindow: true }).then(function (tabs) {
    tab = tabs[0];
    if (!tab) return;
    return chrome.storage.session.get('tab:' + tab.id).then(function (v) {
      captured = v['tab:' + tab.id] || [];
      render();
    });
  });
  D.health().then(function (h) { $('dm').textContent = 'núcleo ' + h.version; }).catch(connectionHint);
})();
