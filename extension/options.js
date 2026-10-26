(function () {
  'use strict';
  var $ = function (id) { return document.getElementById(id); };
  function say(t, bad) { var s = $('st'); s.textContent = t; s.className = bad ? 'bad' : ''; }

  chrome.storage.local.get(['base']).then(function (v) { if (v.base) $('conn').placeholder = v.base + '/#token=… (já configurado)'; });

  // cookies: opt-in, com a permissão opcional pedida num gesto do utilizador
  function ckSay(t, bad) { var s = $('ckst'); s.textContent = t; s.className = bad ? 'bad' : ''; }
  Promise.all([chrome.storage.local.get('sendCookies'), chrome.permissions.contains({ permissions: ['cookies'] })])
    .then(function (r) { $('ck').checked = !!r[0].sendCookies && !!r[1]; });
  $('ck').onchange = function () {
    if ($('ck').checked) {
      chrome.permissions.request({ permissions: ['cookies'] }).then(function (granted) {
        $('ck').checked = granted;
        return chrome.storage.local.set({ sendCookies: granted }).then(function () {
          ckSay(granted ? 'Ativado: os cookies do site do link seguem para o StreamVault.' : 'Permissão recusada: nada foi ativado.', !granted);
        });
      }).catch(function () { $('ck').checked = false; ckSay('Não foi possível pedir a permissão.', true); });
    } else {
      chrome.storage.local.set({ sendCookies: false })
        .then(function () { return chrome.permissions.remove({ permissions: ['cookies'] }); })
        .then(function () { ckSay('Desativado e permissão devolvida ao navegador.'); })
        .catch(function () { ckSay('Desativado.'); });
    }
  };

  $('save').onclick = function () {
    var c = self.SVClassify.parseConnection($('conn').value);
    if (!c) { say('Endereço inválido. Cole a linha “Abra: …” do StreamVault (só 127.0.0.1/localhost, com #token=…).', true); return; }
    chrome.storage.local.set({ base: c.base, token: c.token }).then(function () {
      return self.SVDaemon.health();
    }).then(function (h) { say('Ligado ao StreamVault ' + h.version + '.'); $('conn').value = ''; })
      .catch(function () { say('Guardado, mas o StreamVault não respondeu. Confirme que está a correr e que a chave é a atual.', true); });
  };
})();
