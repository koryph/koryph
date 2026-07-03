// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

import * as assert from 'assert';
import { transcriptHtml } from '../../webview/transcriptHtml';

describe('transcriptHtml — strict CSP', () => {
  const html = transcriptHtml('N0NCE', 'vscode-webview://abc');

  it('denies everything by default and pins script/style to the nonce', () => {
    assert.ok(html.includes(`default-src 'none'`), 'default-src none');
    assert.ok(html.includes(`script-src 'nonce-N0NCE'`), 'script nonce');
    assert.ok(html.includes(`style-src 'nonce-N0NCE'`), 'style nonce');
  });

  it('never permits unsafe-inline or remote script/connect origins', () => {
    assert.ok(!html.includes('unsafe-inline'), 'no unsafe-inline');
    assert.ok(!html.includes('unsafe-eval'), 'no unsafe-eval');
    assert.ok(!/connect-src/.test(html), 'no connect-src (the host pushes data in)');
    assert.ok(!/https?:\/\/(?!)/.test(html.split('</head>')[0]), 'no remote origins in CSP');
  });

  it('tags both inline blocks with the nonce', () => {
    assert.ok(html.includes('<style nonce="N0NCE">'));
    assert.ok(html.includes('<script nonce="N0NCE">'));
  });

  it('scopes img/font to the provided cspSource only', () => {
    assert.ok(html.includes('img-src vscode-webview://abc'));
    assert.ok(html.includes('font-src vscode-webview://abc'));
  });
});
