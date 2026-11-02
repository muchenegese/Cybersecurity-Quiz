/* ============================================================================
   KwiqTest — client application

   Deliberately dependency-free: no framework, no build step, no CDN. The whole
   app is this file plus one stylesheet, which is what lets the server ship as a
   single portable binary and lets a phone on a weak conference-centre network
   load the join screen in one round trip.

   Realtime state arrives over Server-Sent Events. Every frame is a *complete*
   snapshot rather than a delta, so a client that misses frames — a backgrounded
   phone tab, a lift with no signal — is fully correct again the moment the next
   frame lands, with no replay protocol.
   ========================================================================== */
(function () {
  'use strict';

  /* ----------------------------------------------------------- DOM utils -- */

  var SVG_NS = 'http://www.w3.org/2000/svg';

  /** h('div.card', {onclick: fn}, 'text', childNode) */
  function h(spec, props) {
    var parts = spec.split(/(?=[.#])/);
    var tag = parts[0] || 'div';
    var node = tag === 'svg' || tag === 'circle'
      ? document.createElementNS(SVG_NS, tag)
      : document.createElement(tag);
    for (var i = 1; i < parts.length; i++) {
      var v = parts[i].slice(1);
      if (parts[i][0] === '.') node.classList.add(v); else node.id = v;
    }
    props = props || {};
    for (var k in props) {
      var val = props[k];
      if (val === null || val === undefined || val === false) continue;
      if (k === 'class') { String(val).split(/\s+/).forEach(function (c) { if (c) node.classList.add(c); }); }
      else if (k === 'style' && typeof val === 'object') { for (var s in val) node.style[s] = val[s]; }
      else if (k === 'html') node.innerHTML = val;
      else if (k.slice(0, 2) === 'on') node.addEventListener(k.slice(2), val);
      else if (k in node && k !== 'list' && node.namespaceURI !== SVG_NS) node[k] = val;
      else node.setAttribute(k, val);
    }
    for (var a = 2; a < arguments.length; a++) append(node, arguments[a]);
    return node;
  }

  function append(parent, child) {
    if (child === null || child === undefined || child === false) return;
    if (Array.isArray(child)) { child.forEach(function (c) { append(parent, c); }); return; }
    parent.appendChild(child.nodeType ? child : document.createTextNode(String(child)));
  }

  function clear(node) { while (node.firstChild) node.removeChild(node.firstChild); }
  function $(sel, root) { return (root || document).querySelector(sel); }
  function mount(node) { var app = $('#app'); clear(app); append(app, node); return node; }

  /* -------------------------------------------------------------- state -- */

  var LETTERS = ['A', 'B', 'C', 'D', 'E', 'F'];
  var GLYPHS = ['▲', '◆', '●', '■', '★', '✦']; // shape backup for colour
  var MAX_OPTIONS = 6;

  var store = {
    get: function (k, dflt) {
      try { var v = localStorage.getItem(k); return v === null ? dflt : JSON.parse(v); }
      catch (e) { return dflt; }
    },
    set: function (k, v) { try { localStorage.setItem(k, JSON.stringify(v)); } catch (e) {} },
    del: function (k) { try { localStorage.removeItem(k); } catch (e) {} }
  };

  /* The organiser's identity is a client-generated bearer token. It is what
     makes "host without logging in" work: no account, no email, no password —
     but quizzes and reports are still private to whoever holds the token. */
  function hostToken() {
    var t = store.get('kq.host');
    if (!t || t.length < 32) {
      var buf = new Uint8Array(16);
      (window.crypto || {}).getRandomValues
        ? window.crypto.getRandomValues(buf)
        : buf.forEach(function (_, i) { buf[i] = Math.floor(Math.random() * 256); });
      t = Array.prototype.map.call(buf, function (b) {
        return ('0' + b.toString(16)).slice(-2);
      }).join('');
      store.set('kq.host', t);
    }
    return t;
  }

  function playerKey(code) { return 'kq.play.' + code; }
  function getPlayer(code) { return store.get(playerKey(code), null); }
  function setPlayer(code, p) { store.set(playerKey(code), p); }

  /* ---------------------------------------------------------------- api -- */

  function api(path, opts) {
    opts = opts || {};
    var headers = opts.headers || {};
    headers['X-Host-Token'] = hostToken();
    var init = { method: opts.method || 'GET', headers: headers };
    if (opts.body !== undefined) {
      if (opts.body instanceof FormData) { init.body = opts.body; }
      else { headers['Content-Type'] = 'application/json'; init.body = JSON.stringify(opts.body); }
    }
    if (opts.playerToken) headers['X-Player-Token'] = opts.playerToken;
    return fetch(path, init).then(function (res) {
      if (res.status === 204) return null;
      var ct = res.headers.get('content-type') || '';
      var parse = ct.indexOf('json') >= 0 ? res.json() : res.text();
      return parse.then(function (data) {
        if (!res.ok) {
          var msg = (data && data.error) || (typeof data === 'string' && data) || ('Request failed (' + res.status + ')');
          var err = new Error(msg); err.status = res.status; throw err;
        }
        return data;
      });
    });
  }

  /* ------------------------------------------------------------- toasts -- */

  function toast(msg, kind) {
    var host = $('#toasts');
    if (!host) return;
    var t = h('div.toast' + (kind ? '.' + kind : ''), {}, msg);
    host.appendChild(t);
    setTimeout(function () {
      t.style.transition = 'opacity .3s, transform .3s';
      t.style.opacity = '0'; t.style.transform = 'translateY(8px)';
      setTimeout(function () { t.remove(); }, 320);
    }, kind === 'err' ? 4200 : 2600);
  }

  function fail(e) { toast(e && e.message ? e.message : 'Something went wrong', 'err'); }

  /* ---------------------------------------------------------- live feed -- */

  /* Live wraps EventSource and keeps a running estimate of the server clock.
     The countdown a participant sees is derived from the server's deadline and
     this offset, never from the device clock, so changing the phone's time
     changes nothing. */
  /** Monotonic millisecond source, immune to the wall clock moving. */
  function mono() {
    return (window.performance && performance.now) ? performance.now() : Date.now();
  }

  function Live(code, role, opts) {
    var self = this;
    opts = opts || {};
    this.serverAtAnchor = null;
    this.anchorAt = 0;
    this.onstate = opts.onstate || function () {};
    this.onstatus = opts.onstatus || function () {};
    this.closed = false;

    var qs = 'role=' + encodeURIComponent(role);
    if (role === 'host') qs += '&host=' + encodeURIComponent(hostToken());
    if (opts.playerToken) qs += '&token=' + encodeURIComponent(opts.playerToken);

    var es = new EventSource('/api/sessions/' + encodeURIComponent(code) + '/events?' + qs);
    this.es = es;

    this.status = 'connecting';
    // Only report genuine transitions: a status callback on every frame would
    // make listeners re-render constantly for no reason.
    function setStatus(s) {
      if (self.status === s || self.closed) return;
      self.status = s;
      self.onstatus(s);
    }

    es.addEventListener('open', function () { setStatus('online'); });
    es.addEventListener('error', function () {
      if (!self.closed) setStatus(es.readyState === 2 ? 'closed' : 'reconnecting');
    });
    es.addEventListener('state', function (ev) {
      var st;
      try { st = JSON.parse(ev.data); } catch (e) { return; }
      // Re-anchor the server clock against a monotonic reading. The device's
      // wall clock is never consulted, so neither a user changing the time nor
      // an NTP correction mid-question can disturb the countdown.
      if (typeof st.serverNow === 'number') {
        self.serverAtAnchor = st.serverNow;
        self.anchorAt = mono();
      }
      // State first, then status: a listener woken by the status change must
      // never render against the previous snapshot.
      self.onstate(st);
      setStatus('online');
    });
  }
  /** Best estimate of the server's clock right now, in server milliseconds. */
  Live.prototype.now = function () {
    if (this.serverAtAnchor === null) return Date.now();
    return this.serverAtAnchor + (mono() - this.anchorAt);
  };
  Live.prototype.close = function () { this.closed = true; try { this.es.close(); } catch (e) {} };

  /* ------------------------------------------------------- shared bits --- */

  function brandmark(href) {
    return h('a.brandmark', { href: href || '#/', style: { textDecoration: 'none', color: 'inherit', display: 'flex', alignItems: 'center', gap: '10px' } },
      h('img', { src: '/assets/logo.png', alt: 'Logo', style: { height: '32px' } }),
      'Cybersecurity Quiz'); 
  }

  function statusChip(status) {
    var map = {
      online: ['chip--live', 'Live'],
      reconnecting: ['chip--warn', 'Reconnecting…'],
      closed: ['chip--warn', 'Disconnected']
    };
    var m = map[status] || map.reconnecting;
    return h('span.chip.' + m[0], {}, h('span.pulse'), m[1]);
  }

  function mediaBlock(url, kind, mime, small) {
    if (!url) return null;
    var box = h('div.media-block' + (small ? '.media-block--sm' : ''), { style: { position: 'relative' } });
    if (kind === 'video') {
      var v = h('video', {
        src: url, autoplay: true, muted: true, playsInline: true, loop: false,
        controls: false, preload: 'auto', style: { maxWidth: '100%' }
      });
      v.muted = true;                    // property, not just attribute — iOS
      v.setAttribute('playsinline', '');  // required for inline playback on iPhone
      append(box, v);
      var unmute = h('button.video-hint', {
        type: 'button', 'aria-label': 'Unmute video',
        onclick: function () {
          v.muted = !v.muted;
          unmute.textContent = v.muted ? '\u{1F507} Tap for sound' : '\u{1F50A} Sound on';
          if (!v.muted) v.play().catch(function () {});
        }
      }, '\u{1F507} Tap for sound');
      append(box, unmute);
      // If the file cannot play at all, say so rather than leaving a black box.
      v.addEventListener('error', function () {
        clear(box);
        append(box, h('div.panel.center', { style: { width: '100%' } },
          h('p.muted.small', {}, 'Video could not load on this device — the question still counts.')));
      });
      v.play().catch(function () { /* autoplay refused; the poster still shows */ });
    } else {
      append(box, h('img', { src: url, alt: '', loading: 'eager', decoding: 'async' }));
    }
    return box;
  }

  /* Timer ring driven by one shared animation frame loop. */
  function timerRing(size) {
    size = size || 66;
    var R = 29, C = 2 * Math.PI * R;
    var value = h('circle.value', { cx: 33, cy: 33, r: R, 'stroke-dasharray': C, 'stroke-dashoffset': 0 });
    var num = h('div.num', {}, '–');
    var wrap = h('div.timer', {},
      h('div.timer-ring', { style: { width: size + 'px', height: size + 'px' } },
        h('svg', { viewBox: '0 0 66 66' }, h('circle.track', { cx: 33, cy: 33, r: R }), value),
        num));
    wrap.set = function (remainMs, totalMs) {
      var secs = Math.max(0, Math.ceil(remainMs / 1000));
      var frac = totalMs > 0 ? Math.max(0, Math.min(1, remainMs / totalMs)) : 0;
      num.textContent = secs;
      value.setAttribute('stroke-dashoffset', String(C * (1 - frac)));
      wrap.classList.toggle('is-warn', remainMs <= 10000 && remainMs > 5000);
      wrap.classList.toggle('is-urgent', remainMs <= 5000);
    };
    return wrap;
  }

  var ticking = [];
  function onTick(fn) { ticking.push(fn); return function () { var i = ticking.indexOf(fn); if (i >= 0) ticking.splice(i, 1); }; }
  (function loop() {
    for (var i = 0; i < ticking.length; i++) { try { ticking[i](); } catch (e) {} }
    requestAnimationFrame(loop);
  })();

  function leaderboard(rows, youId, limit) {
    if (!rows || !rows.length) {
      return h('p.muted.small.center', {}, 'No scores yet.');
    }
    var list = limit ? rows.slice(0, limit) : rows;
    // An unlimited board is the full, potentially long, roster (the final
    // results screen) — it scrolls in place; a top-N slice is already short
    // enough to never need it.
    var cls = limit ? 'div.board' : 'div.board.scroll-panel.scroll-panel--lg';
    return h(cls, {}, list.map(function (r) {
      return h('div.board-row' + (r.id === youId ? '.is-you' : ''), { 'data-rank': r.rank },
        h('div.rank', {}, r.rank),
        h('div.who', {},
          h('span', { style: { fontSize: '1.2rem' } }, r.avatar || '\u{1F464}'),
          h('div', { style: { minWidth: 0 } },
            h('div.nm', {}, r.name),
            h('div.sub', {}, r.correct + '/' + r.answered + ' correct · ' +
              (r.avgMs ? (r.avgMs / 1000).toFixed(1) + 's avg' : '—')))),
        h('div.pts', {}, r.score));
    }));
  }

  function distribution(question, reveal) {
    if (!question || !reveal) return null;
    var total = 0;
    reveal.counts.forEach(function (c) { total += c; });
    return h('div.dist', {}, question.options.map(function (o, i) {
      var n = reveal.counts[i] || 0;
      var pct = total ? Math.round(n / total * 100) : 0;
      return h('div.dist-row' + (i === reveal.correctIndex ? '.is-correct' : ''), { 'data-opt': i },
        h('div.lbl', {}, LETTERS[i]),
        h('div.dist-bar', {}, h('i', { style: { width: pct + '%' } })),
        h('div.val', {}, n + ' · ' + pct + '%'));
    }));
  }

  /* ------------------------------------------------- dashboard building blocks */

  /** A centred, promise-based confirmation dialog — used for anything
      destructive (deleting a test) instead of the browser's native confirm(),
      which looks out of place next to the rest of the organiser console. */
  function confirmDialog(opts) {
    opts = opts || {};
    return new Promise(function (resolve) {
      function close(result) {
        document.removeEventListener('keydown', onKey);
        backdrop.remove();
        resolve(result);
      }
      function onKey(e) { if (e.key === 'Escape') close(false); }
      var backdrop = h('div.overlay-backdrop.overlay-backdrop--center', { onclick: function () { close(false); } },
        h('div.card.modal.stack', {
          role: 'alertdialog', 'aria-modal': 'true', 'aria-label': opts.title || 'Confirm',
          onclick: function (e) { e.stopPropagation(); }
        },
          h('h3', { style: { margin: 0 } }, opts.title || 'Are you sure?'),
          opts.body ? h('p.muted.small', { style: { margin: 0 } }, opts.body) : null,
          h('div.btn-group', { style: { justifyContent: 'flex-end' } },
            h('button.btn.btn--ghost', { onclick: function () { close(false); } }, opts.cancelLabel || 'Cancel'),
            h('button.btn' + (opts.danger ? '.btn--danger' : '.btn--primary'), {
              onclick: function () { close(true); }
            }, opts.confirmLabel || 'Confirm'))));
      document.body.appendChild(backdrop);
      document.addEventListener('keydown', onKey);
      var confirmBtn = backdrop.querySelector('.btn--primary, .btn--danger');
      if (confirmBtn) confirmBtn.focus();
    });
  }

  /** A row of headline numbers for a dashboard. */
  function kpiRow(items) {
    return h('div.kpi-row', {}, items.map(function (it) {
      return h('div.kpi-tile', {},
        h('div.kpi-value', {}, it.value),
        h('div.kpi-label', {}, it.label));
    }));
  }

  /** A simple underline tab bar. The caller owns state and re-renders. */
  function tabBar(items, activeKey, onSelect) {
    return h('div.tabs', { role: 'tablist' }, items.map(function (it) {
      return h('button.tab' + (it.key === activeKey ? '.is-active' : ''), {
        type: 'button', role: 'tab', 'aria-selected': it.key === activeKey ? 'true' : 'false',
        onclick: function () { onSelect(it.key); }
      }, it.label);
    }));
  }

  /** A small status pill for a session's phase, reusing the chip family. */
  function phaseChip(phase) {
    var map = {
      lobby: ['chip--warn', 'Lobby'], question: ['chip--live', 'Live'],
      reveal: ['chip--live', 'Live'], ended: ['chip', 'Ended']
    };
    var m = map[phase] || ['chip', phase || '—'];
    return h('span.chip.' + m[0], {}, m[1]);
  }

  /** A click-to-sort data table. Manages its own sort/expansion state and
      re-renders in place, independent of the view that created it — the
      candidates table in the session report is the main user, where a row
      click expands that candidate's full per-question answer breakdown. */
  function sortableTable(columns, rows, opts) {
    opts = opts || {};
    var sortKey = opts.defaultSort || (columns[0] && columns[0].key);
    var sortDir = opts.defaultDir || 1;
    var expandedKey = null;
    var wrap = h('div.table-scroll');
    renderTable();

    function renderTable() {
      clear(wrap);
      var sorted = rows.slice().sort(function (a, b) {
        var av = a[sortKey], bv = b[sortKey];
        if (av === bv) return 0;
        return (av > bv ? 1 : -1) * sortDir;
      });
      var thead = h('thead', {}, h('tr', {}, columns.map(function (c) {
        var sortable = c.sortable !== false;
        return h('th' + (sortable ? '.is-sortable' : '') + (c.key === sortKey ? '.is-sorted' : ''), {
          onclick: sortable ? function () {
            if (sortKey === c.key) sortDir = -sortDir; else { sortKey = c.key; sortDir = 1; }
            renderTable();
          } : null
        }, c.label, (sortable && c.key === sortKey) ? h('span.sort-arrow', {}, sortDir === 1 ? ' ▲' : ' ▼') : null);
      })));
      var tbody = h('tbody');
      sorted.forEach(function (row) {
        var key = opts.rowKey ? row[opts.rowKey] : undefined;
        var tr = h('tr' + (opts.expandRender ? '.is-clickable' : ''), {
          onclick: opts.expandRender ? function () {
            expandedKey = expandedKey === key ? null : key;
            renderTable();
          } : null
        }, columns.map(function (c) {
          return h('td', {}, c.render ? c.render(row) : row[c.key]);
        }));
        append(tbody, tr);
        if (opts.expandRender && expandedKey === key) {
          append(tbody, h('tr.data-table-expand', {},
            h('td', { colSpan: columns.length }, opts.expandRender(row))));
        }
      });
      var table = h('table.data-table', {}, thead, tbody);
      append(wrap, table);
    }
    return wrap;
  }

  /** One question's full response breakdown, for the session report's
      Detailed tab. Fed from the /analytics endpoint's precomputed option
      counts rather than a live reveal frame, but drawn with the same
      `.dist`/`.dist-row` bars used during a live quiz for visual continuity. */
  function questionAnalysisCard(q) {
    return h('div.card.stack-2', {},
      h('div.row.row--between', {},
        h('span.eyebrow', {}, 'Question ' + (q.index + 1)),
        h('span.chip', {}, q.points + ' pts · ' + q.timeLimitSec + 's')),
      q.mediaUrl ? mediaBlock(q.mediaUrl, q.mediaKind, '', true) : null,
      h('h3', { style: { margin: 0 } }, q.text || '(media question)'),
      h('div.dist', {}, q.options.map(function (o, oi) {
        return h('div.dist-row' + (o.isCorrect ? '.is-correct' : ''), { 'data-opt': oi },
          h('div.lbl', {}, o.label),
          h('div.dist-bar', {}, h('i', { style: { width: o.pct + '%' } })),
          h('div.val', {}, o.count + ' · ' + o.pct + '%'));
      })),
      h('p.tiny.muted', { style: { margin: 0 } },
        q.answered + ' of ' + (q.answered + q.noAnswer) + ' answered · ' + q.correct + ' correct · avg ' +
        (q.avgMs / 1000).toFixed(1) + 's'));
  }

  /** The export button group offered wherever a session's results can be
      downloaded — the full PDF, a real Excel workbook, and the four CSVs. */
  function exportMenu(code) {
    function dl(fmt, label, primary) {
      return h('a.btn.btn--sm' + (primary ? '.btn--primary' : ''), {
        href: '/api/sessions/' + code + '/report?format=' + fmt + '&host=' + encodeURIComponent(hostToken())
      }, label);
    }
    return h('div.stack-2', {},
      h('h3', { style: { margin: 0 } }, 'Export results'),
      h('div.btn-group', {},
        dl('pdf', '⬇ PDF report', true),
        dl('xlsx', '⬇ Excel workbook', true),
        dl('leaderboard', 'Leaderboard CSV'),
        dl('questions', 'Questions CSV'),
        dl('attendees', 'Attendees CSV'),
        dl('responses', 'Responses CSV')));
  }

  function confetti() {
    if (window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches) return;
    var c = h('canvas', { id: 'confetti' });
    document.body.appendChild(c);
    var ctx = c.getContext('2d'), W = c.width = innerWidth, H = c.height = innerHeight;
    var colors = ['#7b6bff', '#34d399', '#f5c518', '#fb7185', '#38bdf8'];
    var bits = [];
    for (var i = 0; i < 110; i++) {
      bits.push({
        x: Math.random() * W, y: -20 - Math.random() * H * .5,
        r: 4 + Math.random() * 5, c: colors[i % colors.length],
        vy: 2 + Math.random() * 3.4, vx: -1.2 + Math.random() * 2.4,
        a: Math.random() * Math.PI, va: -.14 + Math.random() * .28
      });
    }
    var end = Date.now() + 4200;
    (function draw() {
      ctx.clearRect(0, 0, W, H);
      bits.forEach(function (b) {
        b.y += b.vy; b.x += b.vx; b.a += b.va;
        ctx.save(); ctx.translate(b.x, b.y); ctx.rotate(b.a);
        ctx.fillStyle = b.c; ctx.fillRect(-b.r, -b.r * .5, b.r * 2, b.r);
        ctx.restore();
      });
      if (Date.now() < end) requestAnimationFrame(draw); else c.remove();
    })();
  }

  function copyText(text) {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      return navigator.clipboard.writeText(text);
    }
    var ta = h('textarea', { value: text, style: { position: 'fixed', opacity: '0' } });
    document.body.appendChild(ta); ta.select();
    try { document.execCommand('copy'); } finally { ta.remove(); }
    return Promise.resolve();
  }

  /** "ABC123" -> "ABC 123" — a mid-code gap makes a 6-character code easier to
      read at a glance and to read aloud, without caring whether it is digits,
      letters or both. */
  function formatCode(code) {
    return String(code || '').replace(/^(.{3})(.+)$/, '$1 $2');
  }

  /** A shimmering placeholder shown for the brief window between navigating
      to a view and its first real frame arriving (a quiz loading, a session
      connecting). A blank screen or a lone "Loading…" line reads as stalled;
      blocks roughly the shape of the page that is about to appear read as
      "in progress" instead. */
  function loadingSkeleton() {
    return h('div.stack', { style: { flex: '1', paddingTop: '8px' } },
      h('div.skeleton', { style: { height: '40px', width: '160px' } }),
      h('div.skeleton', { style: { height: '120px', width: '100%' } }),
      h('div.skeleton', { style: { height: '84px', width: '100%' } }),
      h('div.skeleton', { style: { height: '84px', width: '70%' } }));
  }

  /* ============================================================== router == */

  var current = null;   // { destroy() }

  function go(hash) { location.hash = hash; }

  function route() {
    if (current && current.destroy) { try { current.destroy(); } catch (e) {} }
    current = null;
    var raw = location.hash.replace(/^#\/?/, '');
    var seg = raw.split('/').filter(Boolean);
    document.body.classList.remove('bigscreen');
    if (!seg.length) return (current = HomeView());
    switch (seg[0]) {
      case 'build':  return (current = BuilderView(seg[1]));
      case 'host':   return (current = HostView(seg[1]));
      case 'join':   return (current = PlayView(seg[1]));
      case 'screen': return (current = ScreenView(seg[1]));
      case 'tests':  return (current = ResultsListView(seg[1]));
      case 'report': return (current = SessionReportView(seg[1]));
      default:       return (current = HomeView());
    }
  }

  /* =============================================================== home === */

  function HomeView() {
    var codeInput = h('input.input.input--code', {
      type: 'text', inputMode: 'text', autocapitalize: 'characters', pattern: '[A-Za-z0-9]*', maxLength: 6,
      placeholder: 'ABC123', 'aria-label': 'Session join code', autocomplete: 'off', spellcheck: 'false'
    });
    codeInput.addEventListener('input', function () {
      var v = codeInput.value.replace(/[^A-Za-z0-9]/g, '').slice(0, 6).toUpperCase();
      codeInput.value = v;
      if (v.length === 6) joinNow();
    });

    function joinNow() {
      var code = codeInput.value.replace(/[^A-Za-z0-9]/g, '').toUpperCase();
      if (code.length !== 6) { toast('Enter the 6-character code', 'err'); codeInput.focus(); return; }
      api('/api/sessions/' + code).then(function () { go('#/join/' + code); }).catch(fail);
    }

    function launch(quizId) {
      api('/api/quizzes/' + quizId + '/sessions', { method: 'POST' })
        .then(function (s) { go('#/host/' + s.code); })
        .catch(fail);
    }

    function createQuiz() {
      api('/api/quizzes', { method: 'POST', body: { title: '' } })
        .then(function (q) { go('#/build/' + q.id); })
        .catch(fail);
    }

    /* ------------------------------------------------------- dashboard ---- */

    var quizzes = [];
    var sessionsByQuiz = {};
    var kpiSlot = h('div');
    var testsSlot = h('div.stack-2');
    var dashboardCard = h('div.card.stack', { style: { display: 'none' } },
      h('div.row.row--between', {},
        h('div', {}, h('div.eyebrow', {}, 'Organiser'), h('h2', { style: { margin: 0 } }, 'Your tests')),
        h('button.btn.btn--sm.btn--primary', { onclick: createQuiz }, '+ New test')),
      kpiSlot,
      testsSlot);

    function fmtDate(iso) {
      try {
        return new Date(iso).toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
      } catch (e) { return ''; }
    }

    function renderKPIs(sessions) {
      var totalParticipants = 0, accSum = 0, accN = 0;
      sessions.forEach(function (s) {
        totalParticipants += s.participantCount || 0;
        if (s.participantCount) { accSum += s.avgAccuracyPc || 0; accN++; }
      });
      clear(kpiSlot);
      append(kpiSlot, kpiRow([
        { label: 'Tests', value: quizzes.length },
        { label: 'Sessions run', value: sessions.length },
        { label: 'Candidates reached', value: totalParticipants },
        { label: 'Avg accuracy', value: accN ? Math.round(accSum / accN) + '%' : '—' }
      ]));
    }

    function renderTests() {
      clear(testsSlot);
      quizzes.forEach(function (q) {
        var sessions = (sessionsByQuiz[q.id] || []).slice()
          .sort(function (a, b) { return new Date(b.createdAt) - new Date(a.createdAt); });
        var last = sessions[0];
        append(testsSlot, h('div.panel.row.row--between', {},
          h('div', { style: { minWidth: 0 } },
            h('div', { style: { fontWeight: '700' } }, q.title || 'Untitled test'),
            h('div.tiny.muted', {},
              q.questionCount + ' question' + (q.questionCount === 1 ? '' : 's') +
              (sessions.length
                ? ' · ' + sessions.length + ' session' + (sessions.length === 1 ? '' : 's') +
                  ' · last run ' + fmtDate(last.createdAt)
                : ' · never run'))),
          h('div.btn-group', {},
            sessions.length
              ? h('a.btn.btn--sm.btn--ghost', { href: '#/tests/' + q.id + '/results' }, 'Results')
              : null,
            h('a.btn.btn--sm.btn--ghost', { href: '#/build/' + q.id }, 'Edit'),
            h('button.btn.btn--sm.btn--primary', { onclick: function () { launch(q.id); } }, 'Host'),
            h('button.btn.btn--sm.btn--danger', { onclick: function () { removeQuiz(q); } }, 'Delete'))));
      });
    }

    function removeQuiz(q) {
      confirmDialog({
        title: 'Delete "' + (q.title || 'Untitled test') + '"?',
        body: 'This permanently deletes the test and its questions. Results from sessions you’ve ' +
          'already run are not affected and stay downloadable until they expire.',
        confirmLabel: 'Delete test', danger: true
      }).then(function (ok) {
        if (!ok) return;
        api('/api/quizzes/' + q.id, { method: 'DELETE' }).then(function () {
          quizzes = quizzes.filter(function (x) { return x.id !== q.id; });
          toast('Test deleted', 'ok');
          if (!quizzes.length) { dashboardCard.style.display = 'none'; } else { renderTests(); }
        }).catch(fail);
      });
    }

    function loadDashboard() {
      Promise.all([
        api('/api/quizzes').catch(function () { return []; }),
        api('/api/sessions').catch(function () { return []; })
      ]).then(function (res) {
        quizzes = res[0] || [];
        var sessions = res[1] || [];
        if (!quizzes.length) { dashboardCard.style.display = 'none'; return; }
        sessionsByQuiz = {};
        sessions.forEach(function (s) { (sessionsByQuiz[s.quizId] = sessionsByQuiz[s.quizId] || []).push(s); });
        dashboardCard.style.display = '';
        renderKPIs(sessions);
        renderTests();
      });
    }
    loadDashboard();

    return { node: mount(h('div.screen.stack', {},
      h('div.row.row--between', {}, brandmark(), h('span.chip.chip--brand', {}, 'no signup')),

      h('div', {
        style: {
          display: 'grid', gap: '16px',
          gridTemplateColumns: window.innerWidth >= 900 ? '1fr 1fr' : '1fr'
        }
      },
        h('div.card.stack', {},
          h('div', {},
            h('div.eyebrow', {}, 'Joining a quiz?'),
            h('h1', {}, 'Enter your code')),
          codeInput,
          h('button.btn.btn--primary.btn--lg.btn--block', { onclick: joinNow }, 'Join quiz'),
          h('p.tiny.muted.center', { style: { margin: 0 } },
            'Scanning the QR code takes you straight in.')),

        h('div.card.stack', {},
          h('div', {},
            h('div.eyebrow', {}, 'Hosting'),
            h('h2', {}, 'Run a live quiz')),
          h('p.muted.small', { style: { margin: 0 } },
            'Build questions with images or video, share a link or QR code, and watch scores update live. ' +
            'No account needed — everything stays on this machine.'),
          h('button.btn.btn--primary.btn--lg.btn--block', { onclick: createQuiz }, '✨ Create a quiz'))),

      dashboardCard,

      h('p.tiny.dim.center', {},
        'KwiqTest runs entirely from one program on this computer. ',
        h('br'),
        'Participants just need the link — no install, no sign-in.')
    )) };
  }

  /* ============================================================ builder === */

  function BuilderView(quizId) {
    var quiz = null, activeIdx = 0, saveTimer = null, dirty = false, destroyed = false;
    // Which per-option "add a picture" accordions are expanded, keyed by
    // "questionIndex:optionIndex". Kept outside render() so typing, toggling
    // the correct answer, etc. — anything that triggers a re-render — does not
    // collapse a picker the organiser has open.
    var optionMediaOpen = {};
    // Session settings live behind a top-bar gear trigger (a popover) instead
    // of a permanent sidebar card, so the same one-column layout works from a
    // phone up to a wide monitor.
    var settingsPanelOpen = false;
    var root = mount(h('div.screen.stack', {}, loadingSkeleton()));

    document.addEventListener('keydown', onKeyDown);
    function onKeyDown(e) {
      if (e.key !== 'Escape' || destroyed) return;
      if (settingsPanelOpen) { settingsPanelOpen = false; render(); }
    }

    api('/api/quizzes/' + quizId).then(function (q) {
      if (destroyed) return;
      quiz = q;
      if (!quiz.questions || !quiz.questions.length) { quiz.questions = [blankQuestion()]; }
      render();
    }).catch(function (e) { fail(e); go('#/'); });

    function blankQuestion() {
      return {
        id: '', text: '', options: [{ text: '' }, { text: '' }, { text: '' }, { text: '' }],
        correctIndex: 0, timeLimitSec: 20, points: 1000, media: null, timerAfterVideo: false
      };
    }

    function markDirty() {
      dirty = true;
      clearTimeout(saveTimer);
      saveTimer = setTimeout(save, 700);
      var s = $('#saveState'); if (s) s.textContent = 'Saving…';
      refreshChrome();
    }

    /* Patch the bits of the page that reflect edits — the launch-readiness
       indicator — without a full re-render, which would yank focus out of
       the field being typed into. */
    function refreshChrome() {
      if (!quiz) return;
      var probs = problems();

      var slot = $('#launchState');
      if (slot) {
        clear(slot);
        append(slot, probs.length
          ? h('span.chip.chip--warn', {}, probs.length + ' to fix')
          : h('span.chip.chip--live', {}, h('span.pulse'), 'Ready to launch'));
      }
    }

    function save() {
      if (!quiz) return Promise.resolve();
      return api('/api/quizzes/' + quizId, {
        method: 'PUT',
        body: {
          title: quiz.title, description: quiz.description,
          questions: quiz.questions, settings: quiz.settings, cover: quiz.cover || { id: '' }
        }
      }).then(function (updated) {
        dirty = false;
        // Keep server-assigned ids without stomping in-flight edits.
        updated.questions.forEach(function (uq, i) { if (quiz.questions[i]) quiz.questions[i].id = uq.id; });
        var s = $('#saveState'); if (s) s.textContent = 'All changes saved';
      }).catch(function (e) {
        var s = $('#saveState'); if (s) s.textContent = 'Not saved';
        fail(e);
      });
    }

    /* --- validation mirrors the server so problems surface before publish -- */
    function problems() {
      var out = [];
      if (!quiz.title || !quiz.title.trim()) out.push({ q: -1, msg: 'Give your quiz a title' });
      if (!quiz.questions.length) out.push({ q: -1, msg: 'Add at least one question' });
      quiz.questions.forEach(function (q, i) {
        if (!q.text.trim() && !q.media) out.push({ q: i, msg: 'Question ' + (i + 1) + ' needs text or media' });
        var filled = q.options.filter(function (o) { return o.text.trim() || o.media; });
        if (filled.length < 2) out.push({ q: i, msg: 'Question ' + (i + 1) + ' needs at least 2 options' });
        // The server rejects a publish if ANY option is left blank, not just if
        // fewer than 2 are filled — surface that here too, or "Ready to launch"
        // lies right up until the 422 comes back from Go live.
        q.options.forEach(function (o, oi) {
          if (!o.text.trim() && !o.media) {
            out.push({ q: i, msg: 'Question ' + (i + 1) + ', option ' + LETTERS[oi] + ' is empty — fill it in or remove it' });
          }
        });
        var c = q.options[q.correctIndex];
        if (!c || (!c.text.trim() && !c.media)) out.push({ q: i, msg: 'Question ' + (i + 1) + ' has no correct answer marked' });
      });
      return out;
    }

    function goLive() {
      var errs = problems();
      if (errs.length) { toast(errs[0].msg, 'err'); if (errs[0].q >= 0) { activeIdx = errs[0].q; render(); } return; }
      save().then(function () {
        return api('/api/quizzes/' + quizId + '/sessions', { method: 'POST' });
      }).then(function (s) { go('#/host/' + s.code); }).catch(fail);
    }

    /* ------------------------------------------------------ media picker -- */

    function mediaPicker(label, currentMedia, accept, onPick) {
      var input = h('input', {
        type: 'file', accept: accept, style: { display: 'none' },
        onchange: function () { if (input.files[0]) upload(input.files[0]); input.value = ''; }
      });
      var drop = h('div.media-drop', {
        role: 'button', tabIndex: 0,
        onclick: function () { input.click(); },
        onkeydown: function (e) { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); input.click(); } },
        ondragover: function (e) { e.preventDefault(); drop.classList.add('is-over'); },
        ondragleave: function () { drop.classList.remove('is-over'); },
        ondrop: function (e) {
          e.preventDefault(); drop.classList.remove('is-over');
          if (e.dataTransfer.files[0]) upload(e.dataTransfer.files[0]);
        }
      }, h('div.small', {}, label), h('div.tiny.dim', {}, 'Tap to choose a file from this device'));

      function upload(file) {
        drop.classList.add('skeleton');
        clear(drop); append(drop, h('div.small', {}, 'Uploading ' + file.name + '…'));
        var fd = new FormData(); fd.append('file', file);
        api('/api/media', { method: 'POST', body: fd })
          .then(function (m) { onPick(m); render(); })
          .catch(function (e) { fail(e); render(); });
      }

      if (currentMedia && currentMedia.id) {
        return h('div.stack-2', {},
          mediaBlock('/media/' + currentMedia.id, currentMedia.kind, currentMedia.mime, true),
          h('div.row.row--tight', {},
            h('span.tiny.muted.grow', {}, currentMedia.name),
            h('button.btn.btn--sm.btn--ghost', { onclick: function () { input.click(); } }, 'Replace'),
            h('button.btn.btn--sm.btn--danger', {
              onclick: function () { onPick(null); render(); }
            }, 'Remove')),
          input);
      }
      return h('div', {}, drop, input);
    }

    /* An option's picture upload is the bulkiest part of an answer row, and a
       question can have up to six of them. Collapsed into a one-line toggle
       by default, it keeps the option list scannable; expanding one reveals
       the full picker (or the existing picture) in place. */
    function mediaAccordion(key, currentMedia, onToggle, isOpen, pickerNode) {
      var hasMedia = !!(currentMedia && currentMedia.id);
      var summary = h('button.media-accordion-toggle', {
        type: 'button', onclick: onToggle, 'aria-expanded': isOpen ? 'true' : 'false'
      },
        hasMedia
          ? h('img.opt-media-thumb', { src: '/media/' + currentMedia.id, alt: '' })
          : h('span.opt-media-thumb.opt-media-thumb--empty', { 'aria-hidden': 'true' }, '\u{1F5BC}\uFE0F'),
        h('span.grow', {}, hasMedia ? (currentMedia.name || 'Picture added') : 'Add a picture to this option'),
        h('span.chevron', { 'aria-hidden': 'true' }, isOpen ? '\u25B2' : '\u25BC'));
      return h('div.media-accordion' + (isOpen ? '.is-open' : ''), {}, summary, isOpen ? pickerNode : null);
    }

    /* ------------------------------------------------------------ render -- */

    function render() {
      if (!quiz || destroyed) return;
      if (activeIdx >= quiz.questions.length) activeIdx = quiz.questions.length - 1;
      if (activeIdx < 0) activeIdx = 0;
      var probs = problems();

      clear(root);
      append(root, [
        h('div.topbar', {},
          brandmark(),
          h('span.spacer'),
          h('span.tiny.muted', { id: 'saveState' }, dirty ? 'Saving…' : 'All changes saved'),
          h('button.btn.btn--sm.btn--ghost', {
            type: 'button', title: 'Add a new question',
            onclick: function () {
              quiz.questions.push(blankQuestion());
              activeIdx = quiz.questions.length - 1; markDirty(); render();
            }
          }, '+ Add question'),
          h('button.btn.btn--icon.btn--ghost', {
            type: 'button', title: 'Session settings', 'aria-label': 'Session settings',
            'aria-haspopup': 'dialog', 'aria-expanded': settingsPanelOpen ? 'true' : 'false',
            onclick: function () { settingsPanelOpen = true; render(); }
          }, h('span', { 'aria-hidden': 'true' }, '⚙'))),

        h('div.builder-layout.stack', {},
          h('div.card.stack', {},
            h('div.field', {},
              h('label', { for: 'qTitle' }, 'Quiz title'),
              h('input.input#qTitle', {
                value: quiz.title, placeholder: 'Friday team trivia', maxLength: 90,
                oninput: function (e) { quiz.title = e.target.value; markDirty(); }
              })),
            h('div.field', {},
              h('label', { for: 'qDesc' }, 'Description'),
              h('textarea.textarea#qDesc', {
                placeholder: 'Optional — shown on the join screen', maxLength: 300,
                oninput: function (e) { quiz.description = e.target.value; markDirty(); }
              }, quiz.description || ''))),

          questionEditor()),

        settingsPanelOpen ? settingsPopover() : null,

        h('div.sticky-bar.row.row--between', {},
          h('div.small#launchState', {}, probs.length
            ? h('span.chip.chip--warn', {}, probs.length + ' to fix')
            : h('span.chip.chip--live', {}, h('span.pulse'), 'Ready to launch')),
          h('div.btn-group', {},
            h('button.btn.btn--ghost', { onclick: function () { save().then(function () { go('#/'); }); } }, 'Save & exit'),
            h('button.btn.btn--primary', { onclick: goLive }, 'Go live →')))
      ]);
    }

    /* Session settings sit behind the gear icon in the top bar as a small
       popover instead of a permanent card, keeping the editor the sole focus
       of the page. */
    function settingsPopover() {
      var s = quiz.settings || {};
      function close() { settingsPanelOpen = false; render(); }
      function toggle(key, label, hint) {
        return h('label.switch', {},
          h('input', {
            type: 'checkbox', checked: !!s[key],
            onchange: function (e) { s[key] = e.target.checked; markDirty(); }
          }),
          h('span.track'),
          h('span.switch-label', {}, label, hint ? h('div.tiny.dim', {}, hint) : null));
      }
      return h('div.overlay-backdrop', { onclick: close },
        h('div.card.settings-popover.stack-2', {
          role: 'dialog', 'aria-modal': 'true', 'aria-label': 'Session settings',
          onclick: function (e) { e.stopPropagation(); }
        },
          h('div.row.row--between', {},
            h('h3', { style: { margin: 0 } }, 'Session settings'),
            h('button.btn.btn--icon.btn--ghost.btn--sm', { 'aria-label': 'Close', onclick: close }, '✕')),
          toggle('speedBonus', 'Speed bonus', 'Faster correct answers score more'),
          toggle('showLeaderboard', 'Show standings to players', 'Between questions'),
          toggle('showLiveRank', 'Show players their rank', 'On the answer feedback screen'),
          toggle('autoAdvance', 'Auto-advance', 'Move on without waiting for you')));
    }

    function questionEditor() {
      var q = quiz.questions[activeIdx];
      if (!q) return h('div.card', {}, h('p.muted', {}, 'No question selected.'));

      function optionRow(o, i) {
        var mediaKey = activeIdx + ':' + i;
        var mediaIsOpen = !!optionMediaOpen[mediaKey];
        function toggleMedia() { optionMediaOpen[mediaKey] = !mediaIsOpen; render(); }
        return h('div.opt-editor' + (q.correctIndex === i ? '.is-correct' : ''), { 'data-opt': i },
          h('div.tag', {}, LETTERS[i]),
          h('div.stack-2.grow', {},
            h('input.input', {
              value: o.text, placeholder: 'Option ' + LETTERS[i], maxLength: 150,
              'aria-label': 'Option ' + LETTERS[i] + ' text',
              oninput: function (e) { o.text = e.target.value; markDirty(); }
            }),
            mediaAccordion(mediaKey, o.media, toggleMedia, mediaIsOpen,
              mediaPicker('Add a picture to this option', o.media, 'image/*', function (m) { o.media = m; markDirty(); })),
            h('div.row.row--between', {},
              h('label.pick-correct', {},
                h('input', {
                  type: 'radio', name: 'correct-' + activeIdx, checked: q.correctIndex === i,
                  onchange: function () { q.correctIndex = i; markDirty(); render(); }
                }),
                q.correctIndex === i ? 'Correct answer' : 'Mark correct'),
              q.options.length > 2
                ? h('button.btn.btn--sm.btn--danger', {
                    onclick: function () {
                      q.options.splice(i, 1);
                      if (q.correctIndex >= q.options.length) q.correctIndex = q.options.length - 1;
                      markDirty(); render();
                    }
                  }, 'Remove')
                : null)));
      }

      return h('div.builder-main.stack', {},
        h('div.card.stack', {},
          h('div.row.row--between', {},
            h('div.row.row--tight', {},
              h('button.btn.btn--sm.btn--ghost', {
                disabled: activeIdx === 0,
                title: 'Previous question', 'aria-label': 'Previous question',
                onclick: function () { activeIdx--; render(); }
              }, '‹'),
              h('h2', { style: { margin: 0 } }, 'Question ' + (activeIdx + 1) + ' of ' + quiz.questions.length),
              h('button.btn.btn--sm.btn--ghost', {
                disabled: activeIdx === quiz.questions.length - 1,
                title: 'Next question', 'aria-label': 'Next question',
                onclick: function () { activeIdx++; render(); }
              }, '›')),
            h('div.btn-group', {},
              h('button.btn.btn--sm.btn--ghost', {
                disabled: activeIdx === 0,
                title: 'Move up',
                onclick: function () {
                  var t = quiz.questions[activeIdx - 1];
                  quiz.questions[activeIdx - 1] = quiz.questions[activeIdx];
                  quiz.questions[activeIdx] = t; activeIdx--; markDirty(); render();
                }
              }, '↑'),
              h('button.btn.btn--sm.btn--ghost', {
                disabled: activeIdx === quiz.questions.length - 1,
                title: 'Move down',
                onclick: function () {
                  var t = quiz.questions[activeIdx + 1];
                  quiz.questions[activeIdx + 1] = quiz.questions[activeIdx];
                  quiz.questions[activeIdx] = t; activeIdx++; markDirty(); render();
                }
              }, '↓'),
              h('button.btn.btn--sm.btn--ghost', {
                onclick: function () {
                  var copy = JSON.parse(JSON.stringify(q)); copy.id = '';
                  quiz.questions.splice(activeIdx + 1, 0, copy);
                  activeIdx++; markDirty(); render();
                }
              }, 'Duplicate'),
              h('button.btn.btn--sm.btn--danger', {
                disabled: quiz.questions.length <= 1,
                onclick: function () {
                  quiz.questions.splice(activeIdx, 1); markDirty(); render();
                }
              }, 'Delete'))),

          h('div.field', {},
            h('label', { for: 'qText' }, 'Question'),
            h('textarea.textarea#qText', {
              placeholder: 'Which landmark is this?', maxLength: 400,
              oninput: function (e) { q.text = e.target.value; markDirty(); }
            }, q.text)),

          h('div.field', {},
            h('label', {}, 'Image or video prompt (optional)'),
            mediaPicker('Upload an image or video from this device', q.media,
              'image/*,video/mp4,video/webm', function (m) { q.media = m; markDirty(); }),
            h('p.tiny.dim', { style: { margin: 0 } },
              'Uploads only — images up to 5 MB (auto-compressed), video up to 50 MB.')),

          q.media && q.media.kind === 'video'
            ? h('label.switch', {},
                h('input', {
                  type: 'checkbox', checked: !!q.timerAfterVideo,
                  onchange: function (e) { q.timerAfterVideo = e.target.checked; markDirty(); }
                }),
                h('span.track'),
                h('span.switch-label', {}, 'Start the timer after the video',
                  h('div.tiny.dim', {}, 'Players watch first; you press start when the room is ready')))
            : null,

          h('div.divider'),

          h('div.q-meta-grid', {},
            h('div.field', {},
              h('label', { for: 'qTime' }, '⏱ Seconds'),
              h('input.input#qTime', {
                type: 'number', inputmode: 'numeric', min: 5, max: 120, step: 1,
                value: q.timeLimitSec,
                oninput: function (e) {
                  var v = parseInt(e.target.value, 10);
                  q.timeLimitSec = isNaN(v) ? 0 : v;
                  markDirty();
                },
                onblur: function (e) {
                  var v = parseInt(e.target.value, 10);
                  if (isNaN(v)) v = 20;
                  v = Math.max(5, Math.min(120, v));
                  q.timeLimitSec = v; e.target.value = v; markDirty();
                }
              })),
            h('div.field', {},
              h('label', { for: 'qPts' }, '🏆 Points'),
              h('input.input#qPts', {
                type: 'number', inputmode: 'numeric', min: 100, max: 10000, step: 50,
                value: q.points,
                oninput: function (e) {
                  var v = parseInt(e.target.value, 10);
                  q.points = isNaN(v) ? 0 : v;
                  markDirty();
                },
                onblur: function (e) {
                  var v = parseInt(e.target.value, 10);
                  if (isNaN(v)) v = 1000;
                  v = Math.max(100, Math.min(10000, v));
                  q.points = v; e.target.value = v; markDirty();
                }
              })))),

        h('div.card.stack-2', {},
          h('div.row.row--between', {},
            h('h3', { style: { margin: 0 } }, 'Answer options'),
            h('span.tiny.muted', {}, '2 to 6, one correct')),
          q.options.map(optionRow),
          q.options.length < MAX_OPTIONS
            ? h('button.btn.btn--sm.btn--block', {
                onclick: function () { q.options.push({ text: '' }); markDirty(); render(); }
              }, '+ Add option')
            : null));
    }

    return {
      destroy: function () {
        destroyed = true; clearTimeout(saveTimer); if (dirty) save();
        document.removeEventListener('keydown', onKeyDown);
      }
    };
  }

  /* ============================================================= results == */

  function ResultsListView(quizId) {
    var root = mount(h('div.screen.stack', {}, loadingSkeleton()));
    var destroyed = false;

    Promise.all([
      api('/api/quizzes/' + quizId).catch(function () { return null; }),
      api('/api/sessions?quizId=' + encodeURIComponent(quizId)).catch(function () { return []; })
    ]).then(function (res) {
      if (destroyed) return;
      render(res[0], res[1] || []);
    });

    function render(quiz, sessions) {
      if (destroyed) return;
      clear(root);

      var totalCandidates = 0, accSum = 0, accN = 0;
      sessions.forEach(function (s) {
        totalCandidates += s.participantCount || 0;
        if (s.participantCount) { accSum += s.avgAccuracyPc || 0; accN++; }
      });

      var table = sessions.length ? sortableTable([
        { key: 'createdAt', label: 'Date', render: function (r) { return new Date(r.createdAt).toLocaleString(); } },
        { key: 'code', label: 'Code' },
        { key: 'phase', label: 'Status', render: function (r) { return phaseChip(r.phase); } },
        { key: 'participantCount', label: 'Candidates' },
        { key: 'avgScore', label: 'Avg score' },
        { key: 'avgAccuracyPc', label: 'Avg accuracy', render: function (r) { return r.avgAccuracyPc + '%'; } },
        {
          key: 'view', label: '', sortable: false,
          render: function (r) { return h('a.btn.btn--sm.btn--primary', { href: '#/report/' + r.code }, 'View report'); }
        }
      ], sessions, { rowKey: 'code', defaultSort: 'createdAt', defaultDir: -1 }) : null;

      append(root, [
        h('div.topbar', {},
          brandmark(),
          h('span.spacer'),
          h('a.btn.btn--sm.btn--ghost', { href: '#/' }, '← Dashboard')),
        h('div', {},
          h('div.eyebrow', {}, 'Results'),
          h('h1', {}, quiz ? quiz.title : 'Test results')),
        kpiRow([
          { label: 'Sessions run', value: sessions.length },
          { label: 'Candidates reached', value: totalCandidates },
          { label: 'Avg accuracy', value: accN ? Math.round(accSum / accN) + '%' : '—' }
        ]),
        sessions.length
          ? h('div.card', {}, table)
          : h('div.card.center', {}, h('p.muted', {}, 'No sessions have been run for this test yet.'))
      ]);
    }

    return { destroy: function () { destroyed = true; } };
  }

  /* ============================================================== report == */

  function SessionReportView(code) {
    var root = mount(h('div.screen.stack', {}, loadingSkeleton()));
    var destroyed = false, activeTab = 'overview', data = null;

    api('/api/sessions/' + code + '/analytics').then(function (a) {
      if (destroyed) return;
      data = a;
      render();
    }).catch(function (e) { fail(e); go('#/'); });

    function setTab(key) { activeTab = key; render(); }

    function overviewTab() {
      var stats = data.stats;
      return h('div.stack', {},
        kpiRow([
          { label: 'Candidates', value: stats.participants },
          { label: 'Questions', value: stats.questions },
          { label: 'Responses', value: stats.responses },
          { label: 'Accuracy', value: stats.accuracyPc + '%' },
          { label: 'Duration', value: stats.duration }
        ]),
        h('div', {
          style: {
            display: 'grid', gap: '16px', alignItems: 'start',
            gridTemplateColumns: window.innerWidth >= 900 ? 'minmax(0,1fr) 340px' : '1fr'
          }
        },
          h('div.card.stack', {},
            h('h3', { style: { margin: 0 } }, 'Final leaderboard'),
            leaderboard(data.standings, null)),
          h('div.card', {}, exportMenu(code))));
    }

    function candidateAnswers(p) {
      return h('div.stack-2', {},
        h('p.small.muted', { style: { margin: 0 } }, 'Every answer ' + p.name + ' gave, in question order:'),
        h('div.table-scroll', {},
          h('table.data-table', {},
            h('thead', {}, h('tr', {},
              h('th', {}, 'Q'), h('th', {}, 'Their answer'), h('th', {}, 'Result'),
              h('th', {}, 'Time'), h('th', {}, 'Points'))),
            h('tbody', {}, p.answers.map(function (a) {
              return h('tr', {},
                h('td', {}, a.questionIndex + 1),
                h('td', {}, a.selectedText),
                h('td', {}, a.selected < 0 ? 'Missed' : (a.correct ? '✓ Correct' : '✗ Wrong')),
                h('td', {}, a.selected < 0 ? '—' : (a.elapsedMs / 1000).toFixed(1) + 's'),
                h('td', {}, a.points));
            })))));
    }

    function detailedTab() {
      var table = sortableTable([
        { key: 'rank', label: 'Rank' },
        { key: 'name', label: 'Name' },
        { key: 'score', label: 'Score' },
        {
          key: 'accuracyPc', label: 'Accuracy',
          render: function (r) { return r.correct + '/' + r.answered + ' (' + r.accuracyPc + '%)'; }
        },
        { key: 'avgMs', label: 'Avg time', render: function (r) { return r.avgMs ? (r.avgMs / 1000).toFixed(1) + 's' : '—'; } },
        { key: 'device', label: 'Device' },
        { key: 'status', label: 'Status' }
      ], data.participants, { rowKey: 'id', defaultSort: 'rank', defaultDir: 1, expandRender: candidateAnswers });

      return h('div.stack', {},
        h('div.stack-2', {},
          h('h3', { style: { margin: 0 } }, 'Questions'),
          data.questions.length
            ? h('div.stack.scroll-panel.scroll-panel--xl', {}, data.questions.map(questionAnalysisCard))
            : h('div.card', {}, h('p.muted', {}, 'This test has no questions.'))),
        h('div.card.stack', {},
          h('h3', { style: { margin: 0 } }, 'Candidates'),
          data.participants.length
            ? h('p.tiny.muted', { style: { margin: 0 } }, 'Click a row to see that candidate’s full answer breakdown.')
            : null,
          data.participants.length
            ? h('div.scroll-panel.scroll-panel--xl', {}, table)
            : h('p.muted', {}, 'Nobody joined this session.')));
    }

    function render() {
      if (destroyed || !data) return;
      clear(root);
      append(root, [
        h('div.topbar', {},
          brandmark(),
          h('span.chip', {}, 'Code ' + code),
          h('span.spacer'),
          h('a.btn.btn--sm.btn--ghost', { href: '#/' }, '← Dashboard')),
        h('div', {}, h('div.eyebrow', {}, 'Session report'), h('h1', {}, data.session.title)),
        tabBar([{ key: 'overview', label: 'Overview' }, { key: 'detailed', label: 'Detailed' }], activeTab, setTab),
        activeTab === 'overview' ? overviewTab() : detailedTab()
      ]);
    }

    return { destroy: function () { destroyed = true; } };
  }

  /* =============================================================== host === */

  function HostView(code) {
    var st = null, status = 'connecting', destroyed = false, viewKey = '', live;
    var root = mount(h('div.screen.screen--wide.stack', {}, loadingSkeleton()));
    var untick = null;

    live = new Live(code, 'host', {
      onstatus: function (s) { status = s; paint(true); },
      onstate: function (s) { st = s; paint(); }
    });

    function control(action, participantId) {
      return api('/api/sessions/' + code + '/control', {
        method: 'POST', body: { action: action, participantId: participantId || '' }
      }).catch(fail);
    }

    // Anything that changes the shape of the console goes in the key and
    // triggers a rebuild. Fast-moving numbers (answer tally, timer, response
    // distribution) are patched in place instead, so a burst of answers never
    // re-renders the page under the organiser's cursor.
    function keyOf() {
      if (!st) return 'none';
      var paused = st.question && st.question.paused ? 1 : 0;
      return [st.phase, st.index, status, st.participantCount, st.locked, paused].join('|');
    }

    function paint(force) {
      if (destroyed || !st) return;
      var k = keyOf();
      if (force || k !== viewKey) { viewKey = k; build(); }
      else { patch(); }
    }

    /* ---------------------------------------------------------- fragments */

    function header(extra) {
      return h('div.topbar', {},
        brandmark(),
        h('span.chip', {}, 'Code ' + code),
        statusChip(status),
        h('span.spacer'),
        extra || null,
        h('a.btn.btn--sm.btn--ghost', { href: '#/screen/' + code, target: '_blank', rel: 'noopener' },
          '\u{1F5B5} Big screen'));
    }

    function joinPanel() {
      var joinUrl = location.origin + '/j/' + code;
      return h('div.card.stack.center', {},
        h('div.eyebrow', {}, 'Join at ' + location.host),
        h('div.joincode', {}, formatCode(code)),
        h('div.qr-frame', {}, h('img', { src: '/qr/' + code + '.png?size=512', alt: 'QR code to join this quiz', width: 240, height: 240 })),
        h('div.btn-group', { style: { justifyContent: 'center' } },
          h('button.btn.btn--sm', {
            onclick: function () { copyText(joinUrl).then(function () { toast('Join link copied', 'ok'); }); }
          }, 'Copy link'),
          h('a.btn.btn--sm.btn--ghost', { href: '/qr/' + code + '.png?size=1024', download: 'kwiqtest-' + code + '.png' },
            'Download QR')),
        h('p.tiny.dim', { style: { margin: 0, wordBreak: 'break-all' } }, joinUrl));
    }

    function peopleList() {
      var box = h('div.people.scroll-panel.scroll-panel--md#peopleBox');
      fillPeople(box);
      return box;
    }

    function fillPeople(box) {
      var people = (st.participants || []);
      clear(box);
      if (!people.length) {
        append(box, h('p.muted.small', {}, 'Nobody has joined yet — share the code above.'));
        return;
      }
      people.forEach(function (p) {
        append(box, h('div.person' +
          (p.answeredThis ? '.is-answered' : '') + (p.connected ? '' : '.is-offline'), {},
          h('span.face', {}, p.avatar),
          h('span.nm', {}, p.name),
          h('button.btn.btn--sm.btn--ghost', {
            style: { minHeight: '26px', padding: '0 6px' }, title: 'Remove ' + p.name,
            onclick: function () { control('kick', p.id); }
          }, '×')));
      });
    }

    function questionPreview() {
      var q = st.question;
      if (!q) return null;
      return h('div.card.stack', {},
        h('div.row.row--between', {},
          h('span.eyebrow', {}, 'Question ' + (st.index + 1) + ' of ' + st.total),
          h('span.chip', {}, q.points + ' pts')),
        q.media ? mediaBlock(q.media, q.mediaKind, q.mediaMime, true) : null,
        h('h2', {}, q.text || '(media question)'),
        h('div.answers', { 'data-count': q.options.length }, q.options.map(function (o, i) {
          var correct = q.correctIndex === i;
          return h('div.answer' + (correct ? '.is-correct' : ''), { 'data-opt': i },
            h('span.glyph', {}, GLYPHS[i]),
            h('span.body', {}, o.text || null, o.media ? h('img', { src: o.media, alt: '' }) : null),
            correct ? h('span.verdict', { title: 'Correct answer' }, '✓') : null);
        })));
    }

    function timerPanel() {
      var q = st.question;
      if (!q) return null;
      var ring = timerRing();
      var answered = h('div', { style: { fontWeight: '800', fontSize: '1.3rem' } }, '0');
      var bar = h('div.timer-bar', { style: { flex: '1' } }, h('i', { style: { width: '0%' } }));
      var panel = h('div.card.stack', {},
        h('div.row', {}, ring, h('div.grow', {},
          h('div.eyebrow', {}, q.paused ? 'Timer held' : 'Time remaining'),
          h('div.small.muted', {}, q.paused ? 'Players are watching the video' : 'Synced to every device'))),
        h('div.divider'),
        h('div.row.row--between', {},
          h('div', {}, h('div.eyebrow', {}, 'Answered'), answered),
          h('div.grow', {}, bar)));
      panel.refresh = function () {
        var total = st.question.timeLimitSec * 1000;
        var remain = st.question.paused
          ? st.question.remainMs
          : Math.max(0, st.question.deadlineMs - live.now());
        ring.set(remain, total);
        var pc = st.participantCount || 0;
        answered.textContent = (st.answeredCount || 0) + ' / ' + pc;
        bar.firstChild.style.width = (pc ? (st.answeredCount / pc * 100) : 0) + '%';
      };
      panel.refresh();
      return panel;
    }

    function controlsBar() {
      var q = st.question;
      var btns = [];
      if (st.phase === 'lobby') {
        btns.push(h('button.btn.btn--primary.btn--lg.grow', {
          disabled: !st.participantCount,
          onclick: function () { control('start'); }
        }, st.participantCount ? '▶ Start quiz' : 'Waiting for players…'));
        btns.push(h('button.btn', { onclick: function () { control(st.locked ? 'unlock' : 'lock'); } },
          st.locked ? '\u{1F513} Unlock room' : '\u{1F512} Lock room'));
      } else if (st.phase === 'question') {
        if (q && q.paused) {
          btns.push(h('button.btn.btn--primary.btn--lg.grow', { onclick: function () { control('resume'); } },
            '⏱ Start timer'));
        } else {
          btns.push(h('button.btn.grow', { onclick: function () { control('pause'); } }, '⏸ Pause'));
        }
        btns.push(h('button.btn', { onclick: function () { control('skip'); } }, '⏭ Skip'));
        btns.push(h('button.btn', { onclick: function () { control('repeat'); } }, '↻ Repeat'));
        btns.push(h('button.btn.btn--primary', { onclick: function () { control('next'); } }, 'Reveal answer'));
      } else if (st.phase === 'reveal') {
        btns.push(h('button.btn.btn--primary.btn--lg.grow', { onclick: function () { control('next'); } },
          st.index + 1 >= st.total ? '\u{1F3C1} Finish & show results' : 'Next question →'));
        btns.push(h('button.btn', { onclick: function () { control('repeat'); } }, '↻ Replay question'));
      }
      if (st.phase !== 'ended') {
        btns.push(h('button.btn.btn--danger', {
          onclick: function () {
            confirmDialog({
              title: 'End the quiz now?', body: 'Players will see the final results immediately.',
              confirmLabel: 'End quiz', danger: true
            }).then(function (ok) { if (ok) control('end'); });
          }
        }, 'End quiz'));
      }
      return h('div.sticky-bar', {}, h('div.btn-group', { style: { width: '100%' } }, btns));
    }

    function reportCTA() {
      return h('div.card.stack', {},
        h('h3', { style: { margin: 0 } }, 'Full report & analytics'),
        h('p.small.muted', { style: { margin: 0 } },
          'A detailed, per-question and per-candidate breakdown, plus PDF, Excel and CSV exports.'),
        h('a.btn.btn--primary.btn--block', { href: '#/report/' + code }, '📊 Open detailed report'),
        h('p.tiny.dim', { style: { margin: 0 } },
          'Available until this session expires. Reports are generated on the server, so they are identical everywhere.'));
    }

    /* ------------------------------------------------------------- build -- */

    function build() {
      if (untick) { untick(); untick = null; }
      clear(root);

      if (st.phase === 'lobby') {
        append(root, [
          header(h('span.chip.chip--brand', {}, st.participantCount + ' joined')),
          h('div.stack', { style: { display: 'grid', gridTemplateColumns: 'minmax(0,1fr)', gap: '16px' } },
            h('div', { style: { display: 'grid', gap: '16px', alignItems: 'start', gridTemplateColumns: window.innerWidth >= 900 ? '360px minmax(0,1fr)' : '1fr' } },
              joinPanel(),
              h('div.card.stack', {},
                h('div.row.row--between', {},
                  h('h2', { style: { margin: 0 } }, st.title),
                  h('span.chip', {}, st.total + ' questions')),
                st.locked ? h('span.chip.chip--warn', {}, 'Room locked — no new joins') : null,
                h('div.divider'),
                h('div.row.row--between', {},
                  h('h3', { style: { margin: 0 } }, 'In the lobby'),
                  h('span.chip.chip--live', {}, h('span.pulse'), st.participantCount)),
                peopleList(),
                h('p.tiny.dim', {},
                  'There is no cap on how many people can join. Lock the room only if you want to stop new arrivals.')))),
          controlsBar()
        ]);
        return;
      }

      if (st.phase === 'ended') {
        append(root, [
          header(),
          h('div.card.stack.center', {},
            h('div.eyebrow', {}, 'Session complete'),
            h('h1', {}, st.title),
            h('p.muted', {}, st.participantCount + ' participants · ' + st.total + ' questions')),
          h('div', { style: { display: 'grid', gap: '16px', alignItems: 'start', gridTemplateColumns: window.innerWidth >= 900 ? 'minmax(0,1fr) 360px' : '1fr' } },
            h('div.card.stack', {},
              h('h3', { style: { margin: 0 } }, 'Final leaderboard'),
              leaderboard(st.leaderboard, null)),
            reportCTA()),
          h('div.sticky-bar.row.row--between', {},
            h('a.btn.btn--ghost', { href: '#/' }, '← Home'),
            h('a.btn', { href: '#/build/' + (st.quizId || '') }, 'Edit quiz'))
        ]);
        return;
      }

      // question / reveal
      var tp = st.phase === 'question' ? timerPanel() : null;
      append(root, [
        header(h('span.chip.chip--brand', {}, 'Q' + (st.index + 1) + '/' + st.total)),
        h('div', { style: { display: 'grid', gap: '16px', alignItems: 'start', gridTemplateColumns: window.innerWidth >= 900 ? 'minmax(0,1fr) 340px' : '1fr' } },
          questionPreview(),
          h('div.stack', {},
            tp,
            h('div.card.stack-2', {},
              h('h3', { style: { margin: 0 } }, st.phase === 'reveal' ? 'Results' : 'Live responses'),
              distribution(st.question, st.reveal),
              st.reveal && st.reveal.noAnswer
                ? h('p.tiny.muted', { style: { margin: 0 } }, st.reveal.noAnswer + ' did not answer')
                : null),
            
            /* --- CHANGED: Show Top Players during live question AND reveal --- */
            h('div.card.stack-2', {},
              h('h3', { style: { margin: 0 } }, 'Top players'),
              leaderboard(st.leaderboard, null, 5)),

            h('div.card.stack-2', {},
              h('h3', { style: { margin: 0 } }, 'Players'),
              peopleList()))),
        controlsBar()
      ]);

      if (tp) untick = onTick(function () { tp.refresh(); });
    }

    /* Between rebuilds, patch only the numbers that move. */
    function patch() {
      var box = $('#peopleBox');
      if (box) fillPeople(box);
      var d = $('.dist');
      if (d && st.question && st.reveal) {
        var newDist = distribution(st.question, st.reveal);
        d.replaceWith(newDist);
      }
    }

    return {
      destroy: function () {
        destroyed = true;
        if (untick) untick();
        live.close();
      }
    };
  }

  /* ============================================================== play ==== */

  function PlayView(code) {
    var st = null, me = getPlayer(code), status = 'connecting';
    var destroyed = false, viewKey = '', live = null, untick = null, submitting = false;
    var root = mount(h('div.screen.screen--narrow.stack', {}, loadingSkeleton()));

    if (me && me.token) { connect(); } else { renderJoin(); }

    /* ------------------------------------------------------------- join --- */

    function renderJoin(info) {
      clear(root);
      var nameInput = h('input.input', {
        placeholder: 'Your name', maxLength: 24, autocomplete: 'nickname',
        'aria-label': 'Your display name', value: (me && me.name) || ''
      });
      var btn = h('button.btn.btn--primary.btn--lg.btn--block', { onclick: submit }, 'Join');

      function submit() {
        var name = nameInput.value.trim();
        if (!name) { toast('Enter a name so the organiser can see you', 'err'); nameInput.focus(); return; }
        btn.disabled = true; btn.textContent = 'Joining…';
        api('/api/sessions/' + code + '/join', {
          method: 'POST', body: { name: name, token: (me && me.token) || '' }
        }).then(function (p) {
          me = p; setPlayer(code, p); connect();
        }).catch(function (e) {
          btn.disabled = false; btn.textContent = 'Join'; fail(e);
        });
      }
      nameInput.addEventListener('keydown', function (e) { if (e.key === 'Enter') submit(); });

      append(root, h('div.stack', { style: { justifyContent: 'center', flex: '1' } },
        h('div.center', {}, brandmark()),
        h('div.card.stack.center', {},
          info && info.cover ? h('img', { src: info.cover, alt: '', style: { borderRadius: '14px' } }) : null,
          h('div.eyebrow', {}, 'You are joining'),
          h('h1', { style: { margin: 0 } }, (info && info.title) || 'Quiz ' + code),
          info && info.participantCount
            ? h('span.chip.chip--live', {}, h('span.pulse'), info.participantCount + ' already here')
            : null,
          h('div.divider'),
          nameInput,
          btn,
          h('p.tiny.dim', { style: { margin: 0 } },
            'No account needed. Your name is the only thing stored.'))));

      if (!info) {
        api('/api/sessions/' + code).then(function (i) {
          if (!destroyed && !live) renderJoin(i);
        }).catch(function () {
          clear(root);
          append(root, h('div.stack.center', { style: { justifyContent: 'center', flex: '1' } },
            h('h1', {}, 'Quiz not found'),
            h('p.muted', {}, 'Session ' + code + ' has ended or the code is wrong.'),
            h('a.btn.btn--primary', { href: '#/' }, 'Try another code')));
        });
      }
    }

    function connect() {
      live = new Live(code, 'participant', {
        playerToken: me.token,
        onstatus: function (s) { status = s; refreshChip(); },
        onstate: function (s) { st = s; paint(); }
      });
    }

    // The connection indicator is always in the DOM — assistive technology and
    // the reconnect logic both depend on it — but during a live question it is
    // only *shown* when something is wrong. A healthy "Live" badge is noise
    // next to a running timer; a "Reconnecting…" badge is vital.
    var chipCompact = false;
    function wrapChip() {
      var c = statusChip(status);
      c.id = 'connChip';
      if (chipCompact && status === 'online') c.classList.add('sr-only');
      return c;
    }
    function refreshChip() {
      var existing = $('#connChip');
      if (existing) existing.replaceWith(wrapChip());
    }

    function keyOf() {
      if (!st) return 'none';
      var you = st.you || {};
      if (st.phase === 'question') return 'q|' + st.index + '|' + (you.answered ? 1 : 0) + '|' + (st.question && st.question.paused ? 1 : 0);
      if (st.phase === 'reveal') return 'r|' + st.index;
      if (st.phase === 'lobby') return 'lobby|' + st.participantCount;
      return 'end';
    }

    function paint() {
      if (destroyed || !st) return;
      var k = keyOf();
      if (k !== viewKey) { viewKey = k; build(); }
    }

    function answer(i) {
      if (submitting || !st || st.phase !== 'question') return;
      if (st.question && st.question.paused) return;
      submitting = true;
      // Lock the UI immediately. The PRD requires answers to be final, and on a
      // slow connection the participant must not be able to tap twice.
      Array.prototype.forEach.call(root.querySelectorAll('.answer'), function (b, bi) {
        b.disabled = true;
        if (bi === i) b.classList.add('is-chosen'); else b.classList.add('is-muted');
      });
      if (navigator.vibrate) { try { navigator.vibrate(18); } catch (e) {} }
      api('/api/sessions/' + code + '/answer', {
        method: 'POST', playerToken: me.token,
        body: { questionIndex: st.index, optionIndex: i }
      }).then(function (res) {
        submitting = false;
        // Apply the server's verdict locally rather than waiting for the next
        // broadcast, so the "locked in" screen appears the instant the request
        // returns even on a slow connection.
        if (st && st.you) {
          st.you.answered = true;
          st.you.selected = i;
          st.you.correct = !!res.correct;
          st.you.gained = res.points;
          st.you.score = res.score;
          st.you.rank = res.rank;
          paint();
        }
      }).catch(function (e) {
        submitting = false;
        toast(e.status === 400 ? 'Too late — time was up' : e.message, 'err');
        viewKey = ''; paint();
      });
    }

    /* ------------------------------------------------------------ build --- */

    function build() {
      if (untick) { untick(); untick = null; }
      clear(root);
      var you = st.you || {};
      chipCompact = st.phase === 'question';

      var bar = h('div.row.row--between', {},
        h('span.chip', {}, (you.avatar || '') + ' ' + (you.name || '')),
        wrapChip(),
        h('span.chip.chip--brand', {}, you.score + ' pts'));

      if (st.phase === 'lobby') {
        append(root, h('div.stack', { style: { flex: '1' } },
          bar,
          h('div.card.stack.center', { style: { flex: '1', justifyContent: 'center' } },
            h('div', { style: { fontSize: '3.4rem' } }, you.avatar || '\u{1F389}'),
            h('h1', { style: { margin: 0 } }, 'You’re in!'),
            h('p.muted', {}, st.title),
            h('span.chip.chip--live', {}, h('span.pulse'), st.participantCount + ' in the lobby'),
            h('div.divider'),
            h('p.small.muted', { style: { margin: 0 } }, 'Hang tight — the organiser starts the quiz.'),
            h('p.tiny.dim', { style: { margin: 0 } }, 'Keep this tab open. If you lose signal you’ll rejoin automatically.'))));
        return;
      }

      if (st.phase === 'ended') {
        var mine = (st.leaderboard || []).filter(function (r) { return r.id === you.id; })[0];
        var rank = (mine && mine.rank) || you.rank;
        if (rank && rank <= 3) confetti();
        append(root, h('div.stack', { style: { flex: '1' } },
          bar,
          h('div.card.stack.center', {},
            h('div.eyebrow', {}, 'Final result'),
            h('div', { style: { fontSize: '3.2rem' } },
              rank === 1 ? '\u{1F947}' : rank === 2 ? '\u{1F948}' : rank === 3 ? '\u{1F949}' : '\u{1F3AF}'),
            h('h1', { style: { margin: 0 } }, 'Rank ' + (rank || '–') + ' of ' + (you.of || st.participantCount)),
            h('p.muted', {}, you.score + ' points'),
            mine ? h('span.chip', {}, mine.correct + '/' + mine.answered + ' correct · ' + mine.accuracyPc + '% accuracy') : null),
          
          /* --- CHANGED: Respect the setting to hide final leaderboard --- */
          st.settings.showLeaderboard && st.leaderboard ? h('div.card.stack-2', {},
            h('h3', { style: { margin: 0 } }, 'Leaderboard'),
            leaderboard(st.leaderboard, you.id)) : null,

          h('a.btn.btn--ghost.btn--block', { href: '#/' }, 'Done')));
        return;
      }

      if (st.phase === 'reveal') {
        var good = you.answered && you.correct;
        append(root, h('div.stack', { style: { flex: '1' } },
          bar,
          h('div.verdict-hero.' + (good ? 'good' : 'bad'), {},
            h('div.mark', {}, good ? '✅' : (you.answered ? '❌' : '⏱')),
            h('div.headline', {}, good ? 'Correct!' : (you.answered ? 'Not this time' : 'Out of time')),
            good ? h('div.gain', {}, '+' + you.gained + ' points') : null,
            st.settings.showLiveRank && you.rank
              ? h('div.muted', {}, 'Rank ' + you.rank + ' of ' + you.of + ' · ' + you.score + ' pts')
              : null),
          st.question ? h('div.card.stack-2', {},
            h('div.eyebrow', {}, 'The answer was'),
            h('div.answer.is-correct', { 'data-opt': st.question.correctIndex },
              h('span.glyph', {}, GLYPHS[st.question.correctIndex]),
              h('span.body', {}, st.question.options[st.question.correctIndex].text || null,
                st.question.options[st.question.correctIndex].media
                  ? h('img', { src: st.question.options[st.question.correctIndex].media, alt: '' }) : null))) : null,
          st.settings.showLeaderboard && st.leaderboard
            ? h('div.card.stack-2', {},
                h('h3', { style: { margin: 0 } }, 'Top players'),
                leaderboard(st.leaderboard, you.id, 5))
            : null,
          h('p.small.muted.center', {},
            st.index + 1 >= st.total ? 'That was the last question…' : 'Get ready for question ' + (st.index + 2) + '…')));
        return;
      }

      /* --- live question --- */
      var q = st.question;
      if (!q) { append(root, h('p.muted', {}, 'Waiting…')); return; }

      var ring = timerRing(58);
      var head = h('div.row.row--between', {},
        h('div.row.row--tight', {}, ring,
          h('div', {},
            h('div.eyebrow', {}, 'Question ' + (st.index + 1) + ' of ' + st.total),
            h('div.tiny.muted', {}, q.points + ' points'))),
        h('div.row.row--tight', {},
          wrapChip(),
          h('span.chip', {}, (you.avatar || '') + ' ' + you.score)));

      if (you.answered) {
        append(root, h('div.stack', { style: { flex: '1' } },
          head,
          h('div.card.stack.center', { style: { flex: '1', justifyContent: 'center' } },
            h('div', { style: { fontSize: '3rem' } }, '\u{1F512}'),
            h('h2', { style: { margin: 0 } }, 'Answer locked in'),
            h('p.muted', {}, 'You picked ' + LETTERS[you.selected] + '. Results in a moment…'),
            h('div.timer-bar', { style: { width: '100%' } }, h('i', { style: { width: '100%' } })))));
      } else if (q.paused) {
        append(root, h('div.stack', { style: { flex: '1' } },
          head,
          q.media ? mediaBlock(q.media, q.mediaKind, q.mediaMime) : null,
          h('div.card.stack.center', {},
            h('h2', { style: { margin: 0 } }, q.text || 'Watch closely…'),
            h('p.muted.small', { style: { margin: 0 } }, 'Answers open when the organiser starts the timer.'))));
      } else {
        append(root, h('div.stack', { style: { flex: '1' } },
          head,
          q.media ? mediaBlock(q.media, q.mediaKind, q.mediaMime) : null,
          q.text ? h('h1', { style: { fontSize: 'clamp(1.25rem,5vw,1.9rem)' } }, q.text) : null,
          h('div.answers', { 'data-count': q.options.length }, q.options.map(function (o, i) {
            return h('button.answer', {
              type: 'button', 'data-opt': i, 'aria-label': 'Option ' + LETTERS[i] + ': ' + (o.text || 'picture answer'),
              onclick: function () { answer(i); }
            },
              h('span.glyph', { 'aria-hidden': 'true' }, GLYPHS[i]),
              h('span.body', {}, o.text || null, o.media ? h('img', { src: o.media, alt: '' }) : null));
          }))));
      }

      untick = onTick(function () {
        if (!st.question) return;
        var total = st.question.timeLimitSec * 1000;
        var remain = st.question.paused ? st.question.remainMs
          : Math.max(0, st.question.deadlineMs - live.now());
        ring.set(remain, total);
      });
    }

    return {
      destroy: function () {
        destroyed = true;
        if (untick) untick();
        if (live) live.close();
      }
    };
  }

  /* =========================================================== bigscreen == */

  function ScreenView(code) {
    var st = null, destroyed = false, viewKey = '', untick = null;
    document.body.classList.add('bigscreen');
    var root = mount(h('div.screen.screen--wide.stack', {}, loadingSkeleton()));

    var live = new Live(code, 'screen', {
      onstate: function (s) { st = s; paint(); }
    });

    function paint() {
      if (destroyed || !st) return;
      var k = [st.phase, st.index, st.participantCount].join('|');
      if (k === viewKey) return;
      viewKey = k;
      build();
    }

    function build() {
      if (untick) { untick(); untick = null; }
      clear(root);

      if (st.phase === 'lobby') {
        append(root, h('div.stack', { style: { flex: '1', justifyContent: 'center' } },
          h('div.center', {}, h('div.eyebrow', {}, 'Join now at ' + location.host)),
          h('div.center', {}, h('div.joincode', {}, formatCode(code))),
          h('div.center', {}, h('div.qr-frame', {},
            h('img', { src: '/qr/' + code + '.png?size=768', alt: 'QR code to join' }))),
          h('h1.center', {}, st.title),
          h('div.center', {}, h('span.chip.chip--live', {}, h('span.pulse'), st.participantCount + ' joined')),
          h('div.people.scroll-panel', { style: { justifyContent: 'center' } }, (st.participants || []).slice(-40).map(function (p) {
            return h('div.person', {}, h('span.face', {}, p.avatar), h('span.nm', {}, p.name));
          }))));
        return;
      }

      if (st.phase === 'ended') {
        confetti();
        append(root, h('div.stack', { style: { flex: '1', justifyContent: 'center' } },
          h('h1.center', {}, '\u{1F3C6} Final results'),
          h('p.center.muted', {}, st.title),
          leaderboard(st.leaderboard, null, 10)));
        return;
      }

      var q = st.question;
      if (!q) return;
      var ring = timerRing(120);
      append(root, h('div.stack', { style: { flex: '1' } },
        h('div.row.row--between', {},
          h('span.chip', {}, 'Question ' + (st.index + 1) + ' / ' + st.total),
          ring,
          h('span.chip.chip--brand', {}, (st.answeredCount || 0) + ' / ' + st.participantCount + ' answered')),
        q.media ? mediaBlock(q.media, q.mediaKind, q.mediaMime) : null,
        h('div.q-text', {}, q.text || ''),
        h('div.answers', { 'data-count': q.options.length }, q.options.map(function (o, i) {
          var isCorrect = st.phase === 'reveal' && q.correctIndex === i;
          var isWrong = st.phase === 'reveal' && q.correctIndex !== i;
          return h('div.answer' + (isCorrect ? '.is-correct' : '') + (isWrong ? '.is-muted' : ''), { 'data-opt': i },
            h('span.glyph', {}, GLYPHS[i]),
            h('span.body', {}, o.text || null, o.media ? h('img', { src: o.media, alt: '' }) : null),
            st.phase === 'reveal' && st.reveal
              ? h('span.tally', {}, st.reveal.counts[i] || 0)
              : null);
        })),
        st.phase === 'reveal' && st.leaderboard
          ? h('div.card.stack-2', {}, h('h3', {}, 'Standings'), leaderboard(st.leaderboard, null, 5))
          : null));

      untick = onTick(function () {
        if (!st.question) return;
        var total = st.question.timeLimitSec * 1000;
        var remain = st.question.paused ? st.question.remainMs
          : Math.max(0, st.question.deadlineMs - live.now());
        ring.set(remain, total);
      });
    }

    return {
      destroy: function () {
        destroyed = true;
        document.body.classList.remove('bigscreen');
        if (untick) untick();
        live.close();
      }
    };
  }

  /* ================================================================ boot == */

  window.addEventListener('hashchange', route);
  // Mobile browsers freeze timers in background tabs; re-sync on return.
  document.addEventListener('visibilitychange', function () {
    if (!document.hidden && current && current.resync) current.resync();
  });
  route();
})();