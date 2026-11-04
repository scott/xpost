// xpost: small progressive enhancements, served from 'self' (strict CSP).
// Everything works without this file except passkey ceremonies, which the
// WebAuthn browser API requires.
document.documentElement.classList.add('js');
document.addEventListener('submit', function (e) {
  var msg = (e.submitter && e.submitter.getAttribute('data-confirm')) || e.target.getAttribute('data-confirm');
  if (msg && !window.confirm(msg)) e.preventDefault();
});
document.addEventListener('click', function (e) {
  var b = e.target.closest('[data-copy]');
  if (!b) return;
  var el = document.getElementById(b.getAttribute('data-copy'));
  if (!el) return;
  el.select();
  if (navigator.clipboard) navigator.clipboard.writeText(el.value).then(function () { b.textContent = 'Copied'; });
});
// Live X-weighted counter. Mirrors internal/weight (the server re-checks on
// save): URLs and {{blog_url}} count 23, CJK and emoji 2, joiners 0.
(function () {
  var scheme = /\bhttps?:\/\/[^\s]+/gi, bare = /\b(?:[a-z0-9-]+\.)+[a-z]{2,}(?:\/[^\s]*)?/gi, dotted = /^https?:\/\/(?:[^\/\s@]+@)?(?:[a-z0-9-]+\.)+[a-z]{2,}(?::\d+)?(?:[\/?#]|$)/i;
  function cw(c) {
    if (c <= 4351 || (c >= 8192 && c <= 8205 && c !== 0x200D) || (c >= 8208 && c <= 8223) || (c >= 8242 && c <= 8247)) return 1;
    if (c === 0x200D || (c >= 0xFE00 && c <= 0xFE0F)) return 0;
    return 2;
  }
  function chars(s) { var n = 0; for (var ch of s) n += cw(ch.codePointAt(0)); return n; }
  function count(t) {
    var n = 0, urls = 0;
    t = t.split('{{blog_url}}');
    n += (t.length - 1) * 23; urls += t.length - 1; t = t.join(' ');
    var rest = t.replace(scheme, function (u) {
      var tr = u.replace(/[.,;:!?)\]}'"]+$/, '');
      n += chars(u.slice(tr.length));
      n += dotted.test(tr) ? 23 : Math.max(chars(tr), 23); urls++; return ' ';
    });
    rest = rest.replace(bare, function (u) { n += Math.max(chars(u), 23); urls++; return ' '; });
    return { n: n + chars(rest) - urls, urls: urls };
  }
  var form = document.querySelector('form[data-edit]');
  if (!form) return;
  var limit = +form.getAttribute('data-limit') || 280;
  var compose = form.hasAttribute('data-compose'), warnAt = +form.getAttribute('data-warn') || 0;
  function sync() {
    var over = 0, any = false, empty = 0;
    form.querySelectorAll('[data-part]').forEach(function (part) {
      var ta = part.querySelector('textarea'), ctr = part.querySelector('[data-counter]');
      if (!ta || !ctr) return;
      var c = count(ta.value);
      ctr.querySelector('[data-n]').textContent = c.n + ' / ' + limit;
      ctr.querySelector('.ring').style.setProperty('--p', Math.min(100, Math.round(c.n * 100 / limit)));
      var note = ctr.querySelector('.note');
      var isOver = c.n > limit, n = +part.getAttribute('data-part');
      ctr.classList.toggle('over', isOver);
      part.classList.toggle('over', isOver);
      if (isOver && !over) over = n;
      if (!compose) {
        if (note) note.textContent = isOver ? (c.n - limit) + ' over. CJK and emoji count 2 each.' : (c.urls ? 'Links count as 23' : (limit - c.n) + ' left');
        return;
      }
      var warn = !isOver && warnAt && c.n > warnAt;
      ctr.classList.toggle('warn', !!warn);
      if (note) note.textContent = isOver ? (c.n - limit) + ' over' : (c.n ? (limit - c.n) + ' left' : '');
      var has = ta.value.trim() !== '' || +part.getAttribute('data-imgs') > 0;
      if (has) any = true; else if (!empty) empty = n;
      var row = part.parentNode.querySelector('[data-mini]');
      if (row) {
        row.querySelector('b').textContent = c.n;
        row.querySelector('.ring').style.setProperty('--p', Math.min(100, Math.round(c.n * 100 / limit)));
        row.classList.toggle('over', isOver);
        row.classList.toggle('warn', !!warn);
      }
    });
    var why = document.querySelector('[data-why]');
    if (compose) {
      // Text reasons are live; the others (time, spacing, cap) are from the
      // last server render, and the server checks everything again.
      var text = !any ? 'Write something to schedule' : over ? 'Part ' + over + ' is over ' + limit : empty ? 'Part ' + empty + ' is empty. Write it or remove it' : '';
      form.querySelectorAll('[data-gate]').forEach(function (b) {
        var other = b.getAttribute('data-other') || '', tail = b.getAttribute('data-tail') || '';
        var r = text && tail && over ? text + ' and ' + tail : (text || other);
        if (b.getAttribute('data-gate') === 'draft' && text) r = text.replace('to schedule', 'to save');
        b.disabled = !!r;
        if (b.getAttribute('data-gate') === 'schedule' && why) why.textContent = r;
      });
      return;
    }
    form.querySelectorAll('[data-save]').forEach(function (b) { b.disabled = !!over; });
    if (why) why.textContent = over ? 'Fix part ' + over + ' to save' : '';
  }
  form.addEventListener('input', sync);
  sync();
})();
// Composer: Cancel asks first when there is text; part rows open in
// place on a phone; account, time and image changes refresh the checks.
(function () {
  var form = document.getElementById('cf');
  if (!form || !form.hasAttribute('data-compose')) return;
  function dirty() {
    var d = !!form.querySelector('.imgrow');
    form.querySelectorAll('textarea').forEach(function (t) { if (t.value.trim()) d = true; });
    return d;
  }
  document.addEventListener('click', function (e) {
    var c = e.target.closest('[data-cancel]');
    if (c) {
      e.preventDefault();
      if (!dirty() || window.confirm('Discard this post? What you wrote is not saved.')) window.location.href = form.getAttribute('data-back');
      return;
    }
    var o = e.target.closest('[data-open]');
    if (o && form.contains(o)) {
      e.preventDefault();
      var i = o.getAttribute('data-open');
      form.querySelectorAll('[data-cpart]').forEach(function (p) { p.classList.toggle('open', p.getAttribute('data-cpart') === i); });
      form.querySelector('input[name=active]').value = i;
      var ta = document.getElementById('text_' + i);
      if (ta) ta.focus();
    }
  });
  form.addEventListener('change', function (e) {
    if (!e.target.matches('[data-refresh]')) return;
    var b = form.querySelector('button[value=check]');
    if (form.requestSubmit) form.requestSubmit(b); else b.click();
  });
  var when = form.querySelector('details[data-ok]');
  if (when && window.matchMedia('(max-width: 760px)').matches) when.removeAttribute('open');
})();
// Passkeys (WebAuthn). Options arrive as JSON with base64url fields; the
// response goes back as a form field so CSRF works as usual.
(function () {
  if (!window.PublicKeyCredential || !navigator.credentials) {
    document.querySelectorAll('[data-passkey-msg]').forEach(function (el) { el.textContent = 'This browser does not support passkeys.'; });
    return;
  }
  function unb64u(s) {
    s = s.replace(/-/g, '+').replace(/_/g, '/');
    while (s.length % 4) s += '=';
    var bin = atob(s), out = new Uint8Array(bin.length);
    for (var i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
    return out.buffer;
  }
  function b64u(buf) {
    var b = new Uint8Array(buf), s = '';
    for (var i = 0; i < b.length; i++) s += String.fromCharCode(b[i]);
    return btoa(s).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
  }
  function decode(pk) {
    pk.challenge = unb64u(pk.challenge);
    if (pk.user) pk.user.id = unb64u(pk.user.id);
    (pk.excludeCredentials || []).forEach(function (c) { c.id = unb64u(c.id); });
    (pk.allowCredentials || []).forEach(function (c) { c.id = unb64u(c.id); });
    return pk;
  }
  function encode(cred) {
    var r = cred.response, out = { id: cred.id, rawId: b64u(cred.rawId), type: cred.type, response: { clientDataJSON: b64u(r.clientDataJSON) },
      clientExtensionResults: cred.getClientExtensionResults ? cred.getClientExtensionResults() : {} };
    if (cred.authenticatorAttachment) out.authenticatorAttachment = cred.authenticatorAttachment;
    if (r.attestationObject) {
      out.response.attestationObject = b64u(r.attestationObject);
      if (r.getTransports) out.response.transports = r.getTransports();
    } else {
      out.response.authenticatorData = b64u(r.authenticatorData);
      out.response.signature = b64u(r.signature);
      if (r.userHandle) out.response.userHandle = b64u(r.userHandle);
    }
    return JSON.stringify(out);
  }
  function post(url, data) {
    return fetch(url, { method: 'POST', credentials: 'same-origin', headers: { Accept: 'application/json' }, body: new URLSearchParams(data) })
      .then(function (res) {
        return res.json().catch(function () { return { error: 'Something went wrong. Try again.' }; }).then(function (j) {
          if (!res.ok) throw new Error(j.error || 'Something went wrong. Try again.');
          return j;
        });
      });
  }
  function say(msg) { var el = document.querySelector('[data-passkey-msg]'); if (el) el.textContent = msg; }
  function friendly(err) {
    if (err && err.name === 'NotAllowedError') return 'Cancelled, or no xpost passkey on this device.';
    if (err && err.name === 'InvalidStateError') return 'This device already has an xpost passkey for you.';
    return (err && err.message) || 'That did not work. Try again.';
  }
  document.querySelectorAll('[data-passkey-login]').forEach(function (btn) {
    btn.disabled = false;
    btn.addEventListener('click', function () {
      var csrf = btn.getAttribute('data-csrf');
      say('');
      post('/login/passkey/begin', { csrf: csrf }).then(function (o) {
        return navigator.credentials.get({ publicKey: decode(o.publicKey) });
      }).then(function (cred) {
        return post('/login/passkey/finish', { csrf: csrf, credential: encode(cred) });
      }).then(function (j) { window.location = j.next || '/queue'; }).catch(function (err) { say(friendly(err)); });
    });
  });
  document.querySelectorAll('[data-passkey-register]').forEach(function (btn) {
    btn.disabled = false;
    btn.addEventListener('click', function () {
      var csrf = btn.getAttribute('data-csrf');
      var name = (document.querySelector('[data-passkey-name]') || {}).value || '';
      say('');
      post('/passkeys/begin', { csrf: csrf }).then(function (o) {
        return navigator.credentials.create({ publicKey: decode(o.publicKey) });
      }).then(function (cred) {
        return post('/passkeys/finish', { csrf: csrf, name: name, credential: encode(cred) });
      }).then(function (j) { window.location = j.next || '/security'; }).catch(function (err) { say(friendly(err)); });
    });
  });
})();
// Rewrite note counter, and Settings: Save for a new webhook waits for both fields.
(function () {
  document.querySelectorAll('textarea[data-count]').forEach(function (ta) {
    var out = document.getElementById(ta.getAttribute('data-count'));
    if (!out) return;
    var max = ta.maxLength > 0 ? ta.maxLength : 280;
    function upd() { out.textContent = Array.from(ta.value).length + ' / ' + max; }
    ta.addEventListener('input', upd); upd();
  });
  document.querySelectorAll('[data-needs]').forEach(function (btn) {
    var id = btn.getAttribute('data-needs');
    var url = document.getElementById('url' + id), key = document.getElementById('key' + id);
    if (!url || !key) return;
    function upd() { btn.disabled = !(url.value.trim() && key.value.trim()); }
    url.addEventListener('input', upd); key.addEventListener('input', upd); upd();
  });
})();
// Banned topic chips: Add topic adds a chip without leaving the form, and
// tapping a chip removes it. Without scripts, unchecking a chip removes the
// topic on Save, and Add topic saves.
document.querySelectorAll('[data-topics]').forEach(function (fs) {
  var box = fs.querySelector('[data-tchips]'), inp = fs.querySelector('[data-topicnew]'), tpl = fs.querySelector('template[data-tchip]');
  if (!box || !inp || !tpl) return;
  function add() {
    var seen = {};
    box.querySelectorAll('input[name=topic]').forEach(function (c) { seen[c.value.toLowerCase()] = true; });
    inp.value.split(/[,\n]/).forEach(function (t) {
      t = t.trim().replace(/\s+/g, ' ');
      if (!t || seen[t.toLowerCase()]) return;
      seen[t.toLowerCase()] = true;
      var chip = tpl.content.firstElementChild.cloneNode(true);
      chip.querySelector('input').value = t;
      chip.querySelector('.tt').textContent = t;
      chip.querySelector('.tx').title = 'Remove ' + t;
      box.appendChild(chip);
    });
    var none = box.querySelector('[data-tnone]');
    if (none && box.querySelector('.tchip')) none.remove();
    inp.value = '';
    inp.focus();
  }
  fs.querySelector('[data-addtopic]').addEventListener('click', function (e) { e.preventDefault(); add(); });
  inp.addEventListener('keydown', function (e) { if (e.key === 'Enter') { e.preventDefault(); add(); } });
  box.addEventListener('change', function (e) {
    var c = e.target.closest('input[name=topic]');
    if (c && !c.checked) c.closest('.tchip').remove();
  });
});
// Cmd+Enter (Mac) or Ctrl+Enter submits the textarea's form through its
// [data-primary-submit] button (or its only submit button), so validation
// and submit handlers run as for a click. Plain Enter still adds a newline.
(function () {
  var mac = /Mac|iPhone|iPad|iPod/.test(navigator.platform || navigator.userAgent || '');
  document.querySelectorAll('[data-kbd-hint]').forEach(function (h) {
    h.textContent = (mac ? '\u2318\u21b5 ' : 'Ctrl+Enter ') + h.getAttribute('data-kbd-hint');
    h.hidden = false;
  });
  function primary(form) {
    var b = form.querySelector('[data-primary-submit]');
    if (b) return b;
    var all = [].filter.call(form.elements, function (el) { return el.matches('button:not([type=button]):not([type=reset]), input[type=submit]') && !el.classList.contains('vh'); });
    return all.length === 1 ? all[0] : null;
  }
  var last = new WeakMap();
  window.addEventListener('pageshow', function () { document.querySelectorAll('form[data-kbd-sent]').forEach(function (f) { f.removeAttribute('data-kbd-sent'); }); });
  document.addEventListener('submit', function (e) { last.set(e.target, e); });
  document.addEventListener('keydown', function (e) {
    if (e.key !== 'Enter' || e.isComposing || e.keyCode === 229 || e.repeat) return;
    if (!(mac ? e.metaKey : e.ctrlKey) || e.altKey || e.shiftKey) return;
    var t = e.target;
    if (!t || t.tagName !== 'TEXTAREA' || t.readOnly || t.disabled || !t.form) return;
    var form = t.form, b = primary(form);
    e.preventDefault();
    if (!b || b.disabled || form.hasAttribute('data-kbd-sent')) return;
    form.setAttribute('data-kbd-sent', '');
    last.delete(form);
    if (form.requestSubmit) form.requestSubmit(b); else b.click();
    // A blocked submit (invalid field, a declined confirm) frees the form again.
    setTimeout(function () { var ev = last.get(form); if (!ev || ev.defaultPrevented) form.removeAttribute('data-kbd-sent'); }, 0);
  });
})();
// Open on X (issue #60): open the x.com tab right inside the click (so no
// popup blocker fires), record the open with fetch, then swap this card or
// the approve flash into its opened state with no reload. If the browser
// blocks the tab, the form submits normally (POST, then a redirect to X).
(function () {
  if (!window.fetch || !window.DOMParser || !window.FormData) return;
  document.addEventListener('submit', function (e) {
    var form = e.target;
    if (e.defaultPrevented || !form.matches || !form.matches('form[data-openx]')) return;
    var url = form.getAttribute('data-openx');
    if (!/^https?:\/\//i.test(url || '')) return;
    e.preventDefault();
    // 'noopener' as a feature makes window.open return null, so open plainly
    // and cut the opener link by hand; null then really means blocked.
    var w = window.open(url, '_blank');
    if (!w) { form.submit(); return; }
    try { w.opener = null; } catch (err) {}
    var btns = form.querySelectorAll('button');
    btns.forEach(function (b) { b.disabled = true; });
    fetch(form.action, {method: 'POST', body: new FormData(form), credentials: 'same-origin',
      headers: {'X-Xpost-Open': '1', 'Accept': 'application/json'}})
      .then(function (r) { return r.json().then(function (j) { if (!r.ok || !j.ok) throw new Error(j.error || 'failed'); return j; }); })
      .then(function (j) {
        return fetch(location.pathname + location.search, {credentials: 'same-origin', headers: {'X-Xpost-Live': '1'}, cache: 'no-store'})
          .then(function (r) { if (!r.ok || r.redirected) throw new Error('page'); return r.text(); })
          .then(function (t) {
            var doc = new DOMParser().parseFromString(t, 'text/html');
            ['d' + j.id, 'rflash'].forEach(function (id) {
              var el = document.getElementById(id), ne = doc.getElementById(id);
              if (el && ne) el.replaceWith(ne);
            });
          });
      })
      .catch(function () { location.reload(); })
      .then(function () { btns.forEach(function (b) { if (b.isConnected) b.disabled = false; }); });
  });
})();
