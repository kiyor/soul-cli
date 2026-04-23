// js/drawer-tmux.js
//
// Declarative tmux drawer powered by Alpine.js.
// Replaces the former imperative renderTmuxDrawer() innerHTML path.
// Single source of truth: Alpine.store('tmux').
//
// HTML binds via `$store.tmux.*` — see the tmux-tab + tmux-drawer block in
// index.html. Opening the drawer starts a 5s poll; closing stops it.
//
// Compat shims: window.toggleTmuxDrawer() and window.loadTmuxSessions() are
// kept as thin wrappers so existing inline onclick / setTimeout bootstrap
// paths still work without changes.

// Auth token resolver. index.html declares `let TOKEN = ...` which creates a
// script-global binding shared across <script> tags in the same document,
// but — unlike `var` — it does NOT attach to `window`. So `window.TOKEN` is
// always undefined here. Use a lexical lookup (wrapped in try/catch in case
// this script somehow runs before the main script has initialized TOKEN),
// with a localStorage fallback.
function weiranToken() {
  try { if (typeof TOKEN !== 'undefined') return TOKEN || ''; } catch (_) {}
  return localStorage.getItem('weiran_token') || '';
}

document.addEventListener('alpine:init', () => {
  Alpine.store('tmux', {
    // ── reactive state ──
    open: false,           // is the drawer open?
    available: false,      // is tmux available on the server?
    sessions: [],          // current tmux sessions
    expanded: {},          // sessionName -> true  (object, not Set, so Alpine can track)
    previewCache: {},      // target ("sess:idx") -> {loading|content|error}
    error: '',             // top-level error message
    lastFetch: 0,
    // ── non-reactive ──
    pollTimer: null,

    get count() { return this.sessions.length; },

    // Toggle the drawer. Starts/stops the 5s auto-refresh poll.
    toggle() {
      this.open = !this.open;
      if (this.open) {
        this.load(true);
        if (this.pollTimer) clearInterval(this.pollTimer);
        this.pollTimer = setInterval(() => this.load(false), 5000);
      } else {
        if (this.pollTimer) { clearInterval(this.pollTimer); this.pollTimer = null; }
      }
    },

    // Fetch /api/tmux/sessions. On `forceRefreshPreviews`, also re-pull
    // previews for any sessions currently expanded.
    async load(forceRefreshPreviews) {
      try {
        const resp = await fetch('/api/tmux/sessions', {
          headers: { 'Authorization': 'Bearer ' + weiranToken() },
        });
        if (!resp.ok) {
          this.error = 'http ' + resp.status;
          return;
        }
        const data = await resp.json();
        this.available = !!data.available;
        this.error = data.error || '';
        this.sessions = data.sessions || [];
        this.lastFetch = Date.now();
        if (forceRefreshPreviews) {
          Object.keys(this.expanded).forEach(name => {
            if (!this.expanded[name]) return;
            const s = this.sessions.find(x => x.name === name);
            if (!s) return;
            const active = (s.windows || []).find(w => w.active) || (s.windows && s.windows[0]);
            if (active) this.loadPreview(name + ':' + active.index, true);
          });
        }
      } catch (e) {
        this.error = 'fetch: ' + e.message;
      }
    },

    toggleSession(name) {
      if (this.expanded[name]) {
        delete this.expanded[name];
      } else {
        this.expanded[name] = true;
        const s = this.sessions.find(x => x.name === name);
        if (s) {
          const active = (s.windows || []).find(w => w.active) || (s.windows && s.windows[0]);
          if (active) {
            const target = name + ':' + active.index;
            if (!(target in this.previewCache)) this.loadPreview(target, false);
          }
        }
      }
    },

    isExpanded(name) { return !!this.expanded[name]; },

    // Always returns a normalized {loading|content|error} object so the
    // template doesn't have to juggle undefined.
    previewFor(name) {
      const s = this.sessions.find(x => x.name === name);
      if (!s) return { loading: true };
      const active = (s.windows || []).find(w => w.active) || (s.windows && s.windows[0]);
      if (!active) return { loading: true };
      return this.previewCache[name + ':' + active.index] || { loading: true };
    },

    previewText(name) {
      const p = this.previewFor(name);
      if (p.error) return p.error;
      if (p.loading || p.content === undefined) return 'loading…';
      return p.content || '(empty)';
    },

    previewClass(name) {
      const p = this.previewFor(name);
      return { loading: !!(p.loading || p.content === undefined), error: !!p.error };
    },

    async loadPreview(target, force) {
      if (!force && (target in this.previewCache)) return;
      this.previewCache[target] = { loading: true };
      try {
        const resp = await fetch(
          '/api/tmux/capture?target=' + encodeURIComponent(target) + '&lines=80',
          { headers: { 'Authorization': 'Bearer ' + weiranToken() } },
        );
        if (!resp.ok) {
          this.previewCache[target] = { error: 'http ' + resp.status };
        } else {
          const data = await resp.json();
          if (data.error) this.previewCache[target] = { error: data.error };
          else            this.previewCache[target] = { content: data.content || '' };
        }
      } catch (e) {
        this.previewCache[target] = { error: e.message };
      }
    },

    // ── display helpers ──
    fmtAge(sec) {
      if (!sec) return '';
      const d = Math.floor((Date.now() / 1000) - sec);
      if (d < 60)    return d + 's ago';
      if (d < 3600)  return Math.floor(d / 60) + 'm ago';
      if (d < 86400) return Math.floor(d / 3600) + 'h ago';
      return Math.floor(d / 86400) + 'd ago';
    },

    shortPath(p) {
      if (!p) return '';
      const home = p.replace(/^\/Users\/[^/]+/, '~');
      if (home.length <= 40) return home;
      const parts = home.split('/');
      if (parts.length <= 3) return home;
      return parts[0] + '/…/' + parts.slice(-2).join('/');
    },

    webUrlFor(w) {
      return w.web_url || ('http://' + location.hostname + ':' + w.web_port);
    },

    webLabelFor(w) {
      return w.web_label || ('port ' + (w.web_port || '?'));
    },
  });
});

// Initial fetch after Alpine finishes processing the DOM. This lets the
// tmux tab appear (or stay hidden) based on server availability even before
// the user opens the drawer. Replaces the old setTimeout(1200ms) bootstrap.
document.addEventListener('alpine:initialized', () => {
  Alpine.store('tmux').load(false).catch(() => {});
});

// ── Compat shims ──
// Legacy call sites (if any remain) can still invoke these globals. They
// forward to the Alpine store and return the underlying promise so callers
// can still .catch() gracefully.
window.toggleTmuxDrawer = function () {
  if (window.Alpine) Alpine.store('tmux').toggle();
};
window.loadTmuxSessions = function (force) {
  if (!window.Alpine) return Promise.resolve();
  return Alpine.store('tmux').load(!!force);
};
