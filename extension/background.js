// Service worker (MV3): observa respostas de rede e guarda, por separador, as
// mídias descarregáveis. O estado vive em chrome.storage.session porque o
// service worker pode ser terminado a qualquer momento.
importScripts('lib/classify.js');

var MAX_PER_TAB = 50;
var queue = Promise.resolve(); // serializa leituras/escritas (eventos chegam em paralelo)

function key(tabId) { return 'tab:' + tabId; }

function updateBadge(tabId, n) {
  chrome.action.setBadgeText({ tabId: tabId, text: n > 0 ? String(n) : '' });
  if (n > 0) chrome.action.setBadgeBackgroundColor({ tabId: tabId, color: '#0b6e8a' });
}

function withTab(tabId, fn) {
  queue = queue.then(function () {
    return chrome.storage.session.get(key(tabId)).then(function (v) {
      var list = fn(v[key(tabId)] || []);
      var o = {}; o[key(tabId)] = list;
      return chrome.storage.session.set(o).then(function () { updateBadge(tabId, list.length); });
    });
  }).catch(function () {});
  return queue;
}

chrome.webRequest.onHeadersReceived.addListener(function (d) {
  if (d.tabId < 0) return;
  var c = self.SVClassify.classifyResponse(d);
  if (!c) return;
  chrome.tabs.get(d.tabId).then(function (tab) {
    c.pageUrl = tab.url || '';
    c.title = tab.title || '';
    c.time = Date.now();
    withTab(d.tabId, function (list) {
      if (list.some(function (x) { return x.url === c.url; })) return list;
      list.push(c);
      return list.slice(-MAX_PER_TAB);
    });
  }).catch(function () {});
}, { urls: ['<all_urls>'], types: ['media', 'xmlhttprequest', 'other'] }, ['responseHeaders']);

// nova navegação no separador: recomeça a lista
chrome.tabs.onUpdated.addListener(function (tabId, info) {
  if (info.status === 'loading' && info.url) withTab(tabId, function () { return []; });
});
chrome.tabs.onRemoved.addListener(function (tabId) {
  queue = queue.then(function () { return chrome.storage.session.remove(key(tabId)); }).catch(function () {});
});
