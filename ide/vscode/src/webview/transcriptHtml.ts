// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// The transcript webview's HTML document + inline client script/style. Kept in
// one place, and free of any `vscode` import, so the CSP and the client-side
// render logic are reviewable (and lightly testable) in isolation. The panel
// (transcriptPanel.ts) supplies the nonce and `webview.cspSource` and streams
// data in over postMessage; nothing is loaded from disk or the network.
//
// Strict CSP (design §4 — "no remote resources"):
//   default-src 'none'         — deny everything by default
//   img-src <cspSource>        — no remote images
//   style-src 'nonce-…'        — only our one inline <style>
//   script-src 'nonce-…'       — only our one inline <script>
//   font-src <cspSource>
// No 'unsafe-inline', no remote origins, no connect-src (the webview never
// fetches — the extension host pushes data in).

/** The header-strip fields (design §4: bead, status, model, attempts, …). */
export interface TranscriptHeader {
  bead: string;
  status: string;
  model: string;
  attempts: number;
  worktree: string;
  project: string;
}

/** Build the full webview HTML. `nonce` must be a fresh per-load random token. */
export function transcriptHtml(nonce: string, cspSource: string): string {
  const csp = [
    `default-src 'none'`,
    `img-src ${cspSource}`,
    `font-src ${cspSource}`,
    `style-src 'nonce-${nonce}'`,
    `script-src 'nonce-${nonce}'`,
  ].join('; ');

  return `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8" />
  <meta http-equiv="Content-Security-Policy" content="${csp}" />
  <meta name="viewport" content="width=device-width, initial-scale=1.0" />
  <title>Koryph Transcript</title>
  <style nonce="${nonce}">${STYLE}</style>
</head>
<body>
  <header id="header">
    <div class="hdr-line">
      <span id="h-bead" class="bead">—</span>
      <span id="h-status" class="pill">—</span>
      <span id="h-model" class="muted">—</span>
      <span id="h-attempts" class="muted"></span>
    </div>
    <div class="hdr-actions">
      <button id="btn-stop" class="btn danger" title="Stop this agent (graceful)">Stop</button>
      <button id="btn-nudge" class="btn" title="Send a nudge to this agent">Nudge…</button>
      <button id="btn-worktree" class="btn" title="Open this agent's worktree">Worktree</button>
      <span class="spacer"></span>
      <label class="toggle" title="Auto-scroll as new output arrives">
        <input type="checkbox" id="follow" checked /> Follow
      </label>
      <span id="spend" class="spend" title="Approximate — the authoritative cost lands at completion">~</span>
    </div>
    <nav class="tabs" role="tablist">
      <button class="tab active" data-tab="transcript" role="tab">Transcript</button>
      <button class="tab" data-tab="stderr" role="tab">stderr.log</button>
      <button class="tab" data-tab="session" role="tab">session.log</button>
    </nav>
  </header>

  <main>
    <section id="pane-transcript" class="pane active" role="tabpanel">
      <div id="transcript"></div>
      <div id="follow-anchor"></div>
    </section>
    <section id="pane-stderr" class="pane" role="tabpanel"><pre id="stderr" class="log"></pre></section>
    <section id="pane-session" class="pane" role="tabpanel"><pre id="session" class="log"></pre></section>
  </main>

  <script nonce="${nonce}">${CLIENT}</script>
</body>
</html>`;
}

// ---------------------------------------------------------------------------
// Inline style — themed via VS Code CSS variables (no hard-coded colors).
// ---------------------------------------------------------------------------

const STYLE = `
:root { color-scheme: light dark; }
* { box-sizing: border-box; }
body {
  margin: 0; padding: 0;
  font-family: var(--vscode-font-family);
  font-size: var(--vscode-font-size);
  color: var(--vscode-foreground);
  background: var(--vscode-editor-background);
}
header {
  position: sticky; top: 0; z-index: 2;
  background: var(--vscode-editor-background);
  border-bottom: 1px solid var(--vscode-panel-border);
  padding: 6px 10px 0;
}
.hdr-line { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
.hdr-actions { display: flex; align-items: center; gap: 8px; margin: 6px 0; flex-wrap: wrap; }
.spacer, .spacer + * { }
.spacer { flex: 1 1 auto; }
.bead { font-weight: 600; }
.muted { color: var(--vscode-descriptionForeground); font-size: 0.9em; }
.pill {
  padding: 1px 8px; border-radius: 10px; font-size: 0.85em;
  background: var(--vscode-badge-background); color: var(--vscode-badge-foreground);
}
.btn {
  font: inherit; padding: 3px 10px; cursor: pointer;
  color: var(--vscode-button-secondaryForeground, var(--vscode-button-foreground));
  background: var(--vscode-button-secondaryBackground, var(--vscode-button-background));
  border: none; border-radius: 3px;
}
.btn:hover { background: var(--vscode-button-secondaryHoverBackground, var(--vscode-button-hoverBackground)); }
.btn.danger { background: var(--vscode-errorForeground); color: var(--vscode-editor-background); }
.toggle { font-size: 0.9em; color: var(--vscode-descriptionForeground); user-select: none; }
.spend { font-size: 0.85em; color: var(--vscode-descriptionForeground); }
.tabs { display: flex; gap: 2px; }
.tab {
  font: inherit; padding: 4px 12px; cursor: pointer; border: none;
  background: transparent; color: var(--vscode-descriptionForeground);
  border-bottom: 2px solid transparent;
}
.tab.active { color: var(--vscode-foreground); border-bottom-color: var(--vscode-focusBorder); }
main { padding: 0; }
.pane { display: none; padding: 10px; }
.pane.active { display: block; }
#transcript { display: flex; flex-direction: column; gap: 8px; }
.item { line-height: 1.5; }
.assistant { white-space: pre-wrap; word-break: break-word; }
.assistant.provisional { opacity: 0.7; }
.assistant.provisional::after { content: '▋'; opacity: 0.6; }
.sys {
  font-size: 0.85em; color: var(--vscode-descriptionForeground);
  border-left: 2px solid var(--vscode-panel-border); padding-left: 8px;
}
details.tool, details.raw { border: 1px solid var(--vscode-panel-border); border-radius: 4px; }
summary { cursor: pointer; padding: 4px 8px; list-style: none; display: flex; gap: 8px; align-items: baseline; }
summary::-webkit-details-marker { display: none; }
.tname { font-weight: 600; }
.tsummary { color: var(--vscode-descriptionForeground); overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.status-dot { font-size: 0.9em; }
.status-pending { color: var(--vscode-descriptionForeground); }
.status-ok { color: var(--vscode-testing-iconPassed, var(--vscode-charts-green, currentColor)); }
.status-error { color: var(--vscode-errorForeground); }
pre.detail, pre.log {
  margin: 0; padding: 8px; overflow: auto; white-space: pre-wrap; word-break: break-word;
  font-family: var(--vscode-editor-font-family, monospace); font-size: 0.9em;
  background: var(--vscode-textCodeBlock-background, transparent);
}
pre.log { white-space: pre; }
footer.result {
  margin-top: 4px; padding: 8px; border-top: 1px solid var(--vscode-panel-border);
  color: var(--vscode-descriptionForeground); font-size: 0.9em;
}
footer.result.error { color: var(--vscode-errorForeground); }
`;

// ---------------------------------------------------------------------------
// Inline client script — receives ops over postMessage and patches the DOM.
// Kept dependency-free and defensive (unknown item kinds render as raw text).
// ---------------------------------------------------------------------------

const CLIENT = `
(function () {
  const vscode = acquireVsCodeApi();
  const root = document.getElementById('transcript');
  const anchor = document.getElementById('follow-anchor');
  const followBox = document.getElementById('follow');
  const spendEl = document.getElementById('spend');
  const nodes = new Map(); // item id -> element

  function esc(s) {
    return String(s == null ? '' : s)
      .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
  }
  function pretty(v) {
    try { return JSON.stringify(v, null, 2); } catch (e) { return String(v); }
  }
  function following() { return followBox && followBox.checked; }
  function scrollIfFollowing() {
    if (following() && anchor && anchor.scrollIntoView) anchor.scrollIntoView({ block: 'end' });
  }

  function render(item) {
    switch (item.kind) {
      case 'system': {
        const el = document.createElement('div');
        el.className = 'item sys';
        const bits = [];
        if (item.model) bits.push('model ' + esc(item.model));
        if (item.sessionId) bits.push('session ' + esc(item.sessionId));
        if (item.cwd) bits.push(esc(item.cwd));
        if (item.tools && item.tools.length) bits.push(item.tools.length + ' tools');
        el.innerHTML = '⚙ ' + bits.join(' · ');
        return el;
      }
      case 'assistant': {
        const el = document.createElement('div');
        el.className = 'item assistant' + (item.provisional ? ' provisional' : '');
        el.textContent = item.text || '';
        return el;
      }
      case 'tool': {
        const el = document.createElement('details');
        el.className = 'item tool';
        const dot = item.status === 'ok' ? '●' : item.status === 'error' ? '✖' : '◌';
        const label = item.name === '(result)' ? 'result' : esc(item.name);
        el.innerHTML =
          '<summary><span class="status-dot status-' + item.status + '">' + dot + '</span>' +
          '<span class="tname">' + label + '</span>' +
          '<span class="tsummary">' + esc(item.inputSummary || item.resultSummary || '') + '</span></summary>';
        const body = document.createElement('div');
        if (item.input !== undefined) {
          const p = document.createElement('pre'); p.className = 'detail';
          p.textContent = 'input:\\n' + pretty(item.input); body.appendChild(p);
        }
        if (item.result !== undefined) {
          const p = document.createElement('pre'); p.className = 'detail';
          p.textContent = 'result:\\n' + (typeof item.result === 'string' ? item.result : pretty(item.result));
          body.appendChild(p);
        }
        el.appendChild(body);
        return el;
      }
      case 'result': {
        const el = document.createElement('footer');
        el.className = 'item result' + (item.isError ? ' error' : '');
        const bits = [];
        if (item.costUsd != null) bits.push('~$' + Number(item.costUsd).toFixed(4));
        if (item.durationMs != null) bits.push((item.durationMs / 1000).toFixed(1) + 's');
        if (item.numTurns != null) bits.push(item.numTurns + ' turns');
        if (item.tokens) {
          const t = [];
          if (item.tokens.input != null) t.push(item.tokens.input + ' in');
          if (item.tokens.output != null) t.push(item.tokens.output + ' out');
          if (t.length) bits.push(t.join(' / ') + ' tok');
        }
        el.textContent = (item.isError ? '✖ ' : '✓ ') + bits.join('  ·  ');
        return el;
      }
      default: {
        const el = document.createElement('details');
        el.className = 'item raw';
        el.innerHTML = '<summary><span class="tname">' + esc(item.type || 'event') +
          '</span><span class="tsummary">raw event</span></summary>';
        const p = document.createElement('pre'); p.className = 'detail';
        p.textContent = pretty(item.raw); el.appendChild(p);
        return el;
      }
    }
  }

  function upsert(item) {
    const next = render(item);
    const prev = nodes.get(item.id);
    if (prev) { prev.replaceWith(next); }
    else { root.appendChild(next); }
    nodes.set(item.id, next);
  }

  function setHeader(h) {
    document.getElementById('h-bead').textContent = h.bead || '—';
    document.getElementById('h-status').textContent = h.status || '—';
    document.getElementById('h-model').textContent = h.model || '';
    document.getElementById('h-attempts').textContent =
      h.attempts ? 'attempt ' + h.attempts : '';
  }

  function setSpend(s) {
    if (!s) return;
    if (s.costUsd != null) spendEl.textContent = '~$' + Number(s.costUsd).toFixed(4) + ' (approx)';
    else if (s.tokens && (s.tokens.input != null || s.tokens.output != null))
      spendEl.textContent = '~' + ((s.tokens.input || 0) + (s.tokens.output || 0)) + ' tok (approx)';
    else spendEl.textContent = '~';
  }

  window.addEventListener('message', function (e) {
    const msg = e.data || {};
    switch (msg.type) {
      case 'header': setHeader(msg.header); break;
      case 'reset': root.innerHTML = ''; nodes.clear(); break;
      case 'ops':
        (msg.ops || []).forEach(function (op) { upsert(op.item); });
        setSpend(msg.spend);
        scrollIfFollowing();
        break;
      case 'log': {
        const el = document.getElementById(msg.tab);
        if (el) { el.textContent = msg.text || ''; }
        break;
      }
    }
  });

  document.querySelectorAll('.tab').forEach(function (tab) {
    tab.addEventListener('click', function () {
      const name = tab.getAttribute('data-tab');
      document.querySelectorAll('.tab').forEach(function (t) { t.classList.remove('active'); });
      document.querySelectorAll('.pane').forEach(function (p) { p.classList.remove('active'); });
      tab.classList.add('active');
      document.getElementById('pane-' + name).classList.add('active');
      vscode.postMessage({ type: 'tab', tab: name });
    });
  });

  document.getElementById('btn-stop').addEventListener('click', function () {
    vscode.postMessage({ type: 'action', action: 'stop' });
  });
  document.getElementById('btn-nudge').addEventListener('click', function () {
    vscode.postMessage({ type: 'action', action: 'nudge' });
  });
  document.getElementById('btn-worktree').addEventListener('click', function () {
    vscode.postMessage({ type: 'action', action: 'worktree' });
  });

  vscode.postMessage({ type: 'ready' });
})();
`;
