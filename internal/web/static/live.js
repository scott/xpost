// xpost live updates (issue #36), served from 'self' under the strict CSP.
// The server only says which draft changed ({seq, kind, id, v}: no text,
// no secrets). This file then refetches the page it is on over the normal
// owner route (marked X-Xpost-Live, so it never counts as activity for the
// session idle timeout) and swaps what changed. A card the owner is working
// in (focused field, typed text, a Reject box they opened) is never
// replaced: it gets an "Updated. Reload card" notice instead.
(function () {
  'use strict';
  var main = document.getElementById('main');
  if (!main || !main.hasAttribute('data-live-page') || !window.fetch || !window.DOMParser) return;
  var page = main.getAttribute('data-live-page');
  var seq = parseInt(main.getAttribute('data-live-seq'), 10) || 0;
  var draftID = parseInt(main.getAttribute('data-live-draft'), 10) || 0;
  var POLL_MS = 15000, HIDDEN_POLL_MS = 60000, DEBOUNCE_MS = 400, NEW_MS = 3000;
  var HDR = {'X-Xpost-Live': '1'};
  var changed = {}, changedAll = false, timer = null, inflight = false, again = false;
  var es = null, hadError = false, errs = [], pollTimer = null, polling = false, stopped = false, leaving = false;
  var snap = {}, pending = {}, userOpen = new WeakSet(), newCount = 0, baseTitle = document.title;

  var sr = document.createElement('div');
  sr.className = 'vh';
  sr.setAttribute('role', 'status');
  sr.setAttribute('aria-live', 'polite');
  document.body.appendChild(sr);

  function each(list, fn) { Array.prototype.forEach.call(list, fn); }
  function remember(root) {
    each(root.querySelectorAll('[data-live]'), function (u) { snap[u.getAttribute('data-live')] = u.outerHTML; });
    each(root.querySelectorAll('[data-live-list]'), function (l) {
      each(l.children, function (c) { if (c.id) snap['#' + c.id] = c.outerHTML; });
    });
  }
  remember(document);

  document.addEventListener('toggle', function (e) {
    var d = e.target;
    if (d.tagName !== 'DETAILS') return;
    if (d.open) userOpen.add(d); else userOpen.delete(d);
  }, true);
  document.addEventListener('submit', function (e) {
    // A form that opens a new tab (Open on X) does not leave this page.
    if (e.target.getAttribute && e.target.getAttribute('target') === '_blank') return;
    setTimeout(function () { if (!e.defaultPrevented) leaving = true; }, 0);
  });

  // busy: the owner is working in el, so it must not be replaced.
  function busy(el) {
    if (leaving) return true;
    var a = document.activeElement;
    if (a && el.contains(a) && (/^(INPUT|TEXTAREA|SELECT)$/.test(a.tagName) || a.isContentEditable)) return true;
    var f = el.querySelectorAll('input, textarea, select');
    for (var i = 0; i < f.length; i++) {
      var x = f[i];
      if (x.type === 'hidden') continue;
      if (x.type === 'checkbox' || x.type === 'radio') { if (x.checked !== x.defaultChecked) return true; continue; }
      if (x.tagName === 'SELECT') {
        for (var j = 0; j < x.options.length; j++) if (x.options[j].selected !== x.options[j].defaultSelected) return true;
        continue;
      }
      if (x.value !== x.defaultValue) return true;
    }
    var ds = el.querySelectorAll('details[open]');
    for (var k = 0; k < ds.length; k++) if (userOpen.has(ds[k])) return true;
    return false;
  }
  // touches: a change event named this card (or a promo nested in it).
  function touches(el) {
    if (changedAll) return true;
    var ids = [el.id].concat(Array.prototype.map.call(el.querySelectorAll('[id^="d"]'), function (n) { return n.id; }));
    for (var i = 0; i < ids.length; i++) { var m = /^d(\d+)$/.exec(ids[i] || ''); if (m && changed[m[1]]) return true; }
    return !el.id && Object.keys(changed).length > 0;
  }
  function hint(el, key, fresh, label) {
    pending[key] = fresh;
    if (el.querySelector('.livehint')) return;
    var h = document.createElement('div');
    h.className = 'hint neutral livehint';
    h.setAttribute('role', 'status');
    var t = document.createElement('span');
    t.textContent = fresh ? 'Updated. ' : 'This moved to another tab. ';
    var b = document.createElement('button');
    b.type = 'button';
    b.className = 'linkbtn';
    b.setAttribute('data-live-reload', key);
    b.textContent = label || 'Reload card';
    t.appendChild(b);
    h.appendChild(t);
    el.insertBefore(h, el.firstChild);
  }
  document.addEventListener('click', function (e) {
    var b = e.target.closest('[data-live-reload]');
    if (!b) return;
    var key = b.getAttribute('data-live-reload');
    var el = key.charAt(0) === '#' ? document.getElementById(key.slice(1)) : document.querySelector('[data-live="' + key + '"]');
    if (!el) return;
    var fresh = pending[key];
    delete pending[key];
    if (fresh) { snap[key] = fresh.outerHTML; el.replaceWith(fresh); } else { delete snap[key]; el.remove(); }
  });

  // Focus survives a swap: refocus the same control in the new element.
  function swap(el, fresh) {
    var a = document.activeElement, path = null;
    if (a && a !== document.body && el.contains(a)) {
      var same = el.querySelectorAll(a.tagName);
      path = [a.tagName, Array.prototype.indexOf.call(same, a)];
    }
    el.replaceWith(fresh);
    if (path) { var t = fresh.querySelectorAll(path[0])[path[1]]; if (t && t.focus) t.focus({preventScroll: true}); }
  }
  // Scroll stays put: the first card or block still on screen keeps its
  // place when cards are added or removed above it.
  function anchor() {
    if (window.scrollY <= 0) return null;
    var c = main.querySelectorAll('[data-live-list] > [id], [data-live]');
    for (var i = 0; i < c.length; i++) {
      var r = c[i].getBoundingClientRect();
      if (r.bottom > 0) return {el: c[i], top: r.top, id: c[i].id};
    }
    return null;
  }
  function restore(a) {
    if (!a) return;
    var el = a.el.isConnected ? a.el : (a.id && document.getElementById(a.id));
    if (!el) return;
    var dy = el.getBoundingClientRect().top - a.top;
    if (Math.abs(dy) > 0.5) window.scrollBy(0, dy);
  }

  function reconcile(list, nl) {
    var old = {}, order = [], desired = [], fresh = [];
    each(list.children, function (c) { if (c.id) { old[c.id] = c; order.push(c); } });
    each(Array.prototype.slice.call(nl.children), function (nc) {
      if (nc.id && old[nc.id]) {
        var oc = old[nc.id], h = nc.outerHTML;
        delete old[nc.id];
        if (h === snap['#' + nc.id]) { desired.push(oc); return; }
        if (busy(oc)) { if (touches(oc)) hint(oc, '#' + nc.id, nc); desired.push(oc); return; }
        snap['#' + nc.id] = h;
        desired.push({swap: oc, by: nc});
      } else if (nc.id) {
        snap['#' + nc.id] = nc.outerHTML;
        nc.classList.add('livenew');
        fresh.push(nc);
        desired.push(nc);
      } else {
        desired.push(nc); // the empty state and end-of-list note
      }
    });
    // Cards no longer in this view go, unless the owner is working in one.
    order.forEach(function (oc, i) {
      if (!old[oc.id]) return;
      if (!busy(oc)) { delete snap['#' + oc.id]; return; }
      hint(oc, '#' + oc.id, null);
      var prev = i > 0 ? order[i - 1] : null, at = -1;
      for (var j = 0; prev && j < desired.length; j++) if (desired[j] === prev || desired[j].swap === prev) at = j;
      desired.splice(at + 1, 0, oc);
    });
    desired.forEach(function (n, i) {
      if (n.swap) { swap(n.swap, n.by); n = desired[i] = n.by; }
      if (list.children[i] !== n) list.insertBefore(n, list.children[i] || null);
    });
    while (list.children.length > desired.length) list.removeChild(list.lastElementChild);
    return fresh;
  }

  function apply(doc) {
    var a = anchor(), fresh = [];
    each(document.querySelectorAll('[data-live]'), function (el) {
      var k = el.getAttribute('data-live'), ne = doc.querySelector('[data-live="' + k + '"]');
      if (!ne) return;
      var h = ne.outerHTML;
      if (h === snap[k]) return;
      if (busy(el)) { if (touches(el)) hint(el, k, ne, 'Reload'); return; }
      snap[k] = h;
      swap(el, ne);
    });
    each(document.querySelectorAll('[data-live-list]'), function (l) {
      var nl = doc.querySelector('[data-live-list="' + l.getAttribute('data-live-list') + '"]');
      if (nl) fresh = fresh.concat(reconcile(l, nl));
    });
    restore(a);
    if (fresh.length) {
      sr.textContent = fresh.length === 1 ? '1 new item' : fresh.length + ' new items';
      if (document.hidden) { newCount += fresh.length; document.title = '(' + newCount + ') ' + baseTitle; }
      setTimeout(function () { fresh.forEach(function (n) { n.classList.remove('livenew'); }); }, NEW_MS);
    }
  }

  function refresh() {
    timer = null;
    if (stopped || leaving) return;
    if (inflight) { again = true; return; }
    inflight = true;
    fetch(location.pathname + location.search, {credentials: 'same-origin', headers: HDR, cache: 'no-store'})
      .then(function (r) {
        if (r.redirected || !r.ok) throw new Error('page ' + r.status);
        return r.text();
      })
      .then(function (t) {
        var doc = new DOMParser().parseFromString(t, 'text/html'), nm = doc.getElementById('main');
        if (!nm || nm.getAttribute('data-live-page') !== page) { stop(); return; }
        apply(doc);
        changed = {};
        changedAll = false;
      })
      .catch(function () {})
      .then(function () {
        inflight = false;
        if (again) { again = false; schedule(); }
      });
  }
  function schedule(all) {
    if (all) changedAll = true;
    if (timer) clearTimeout(timer);
    timer = setTimeout(refresh, DEBOUNCE_MS);
  }

  function pageHint() {
    if (main.querySelector('.livehint')) return;
    var h = document.createElement('div');
    h.className = 'hint warn flash livehint';
    h.setAttribute('role', 'status');
    var t = document.createElement('span');
    t.textContent = 'This draft changed since you opened it. ';
    var b = document.createElement('button');
    b.type = 'button';
    b.className = 'linkbtn';
    b.textContent = 'Reload';
    b.addEventListener('click', function () { location.reload(); });
    t.appendChild(b);
    h.appendChild(t);
    main.insertBefore(h, main.firstChild);
  }
  function onChange(c) {
    if (!c || typeof c.seq !== 'number' || c.seq <= seq) return;
    seq = c.seq;
    if (page === 'draft') { if (c.id === draftID) pageHint(); return; }
    changed[c.id] = true;
    schedule();
  }

  function stop() {
    stopped = true;
    if (es) { es.close(); es = null; }
    if (pollTimer) clearTimeout(pollTimer);
  }
  function startPoll() {
    if (polling || stopped) return;
    polling = true;
    poll();
  }
  function poll() {
    pollTimer = null;
    if (stopped) return;
    fetch('/live/poll?since=' + seq, {credentials: 'same-origin', headers: HDR, cache: 'no-store'})
      .then(function (r) {
        if (r.status === 401) { stop(); return null; }
        return r.ok ? r.json() : null;
      })
      .then(function (j) {
        if (!j) return;
        if (j.resync) { seq = j.seq; if (page === 'draft') pageHint(); else schedule(true); return; }
        (j.changes || []).forEach(onChange);
      })
      .catch(function () {})
      .then(function () { if (!stopped) pollTimer = setTimeout(poll, document.hidden ? HIDDEN_POLL_MS : POLL_MS); });
  }
  function connect() {
    if (stopped) return;
    if (!window.EventSource) { startPoll(); return; }
    es = new EventSource('/live/events?since=' + seq);
    es.addEventListener('change', function (e) { try { onChange(JSON.parse(e.data)); } catch (err) {} });
    es.addEventListener('resync', function () { if (page === 'draft') pageHint(); else schedule(true); });
    es.addEventListener('signedout', stop);
    es.onopen = function () { if (hadError && page !== 'draft') schedule(true); hadError = false; };
    es.onerror = function () {
      hadError = true;
      var now = Date.now();
      errs = errs.filter(function (t) { return now - t < 60000; });
      errs.push(now);
      // Refused (401, 429, a proxy error) or flapping: fall back to polling.
      if (es.readyState === EventSource.CLOSED || errs.length >= 4) { es.close(); es = null; startPoll(); }
    };
  }

  document.addEventListener('visibilitychange', function () {
    if (document.hidden) return;
    newCount = 0;
    document.title = baseTitle;
    if (polling && !stopped) { if (pollTimer) clearTimeout(pollTimer); poll(); }
  });
  window.addEventListener('pagehide', function () { if (es) { es.close(); es = null; } });
  window.addEventListener('pageshow', function (e) {
    if (e.persisted && !stopped) { leaving = false; if (!polling) connect(); if (page !== 'draft') schedule(true); }
  });
  connect();
})();
