// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Unit tests for the project config editor UX (ext.7):
//   • ConfigEditorBanner trigger logic (no `vscode` import — runs headless).
//   • Schema-binding contract: the bundled schema path exists and is parseable
//     as JSON, and package.json `jsonValidation` points to it.
//
// These run under plain mocha (npm test) without an Electron host.

import * as assert from 'assert';
import * as fs from 'fs';
import * as path from 'path';
import {
  CAVEAT_MESSAGE,
  CONFIG_FILENAME,
  ConfigEditorBanner,
  REGISTRY_CAVEAT,
  configFileLabel,
  isProjectConfigPath,
} from '../../config/banner';

// ---------------------------------------------------------------------------
// isProjectConfigPath
// ---------------------------------------------------------------------------

describe('isProjectConfigPath', () => {
  it('matches the exact filename at any depth', () => {
    assert.ok(isProjectConfigPath('koryph.project.json'));
    assert.ok(isProjectConfigPath('/repo/koryph/koryph.project.json'));
    assert.ok(isProjectConfigPath('/a/b/c/koryph.project.json'));
  });

  it('does not match similar filenames', () => {
    assert.ok(!isProjectConfigPath('koryph.project.jsonc'));
    assert.ok(!isProjectConfigPath('other.json'));
    assert.ok(!isProjectConfigPath('koryph.project.json.bak'));
    assert.ok(!isProjectConfigPath(''));
  });
});

// ---------------------------------------------------------------------------
// ConfigEditorBanner — notification trigger and dedup logic
// ---------------------------------------------------------------------------

describe('ConfigEditorBanner', () => {
  function makeSpyBanner(): { banner: ConfigEditorBanner; calls: string[] } {
    const calls: string[] = [];
    const banner = new ConfigEditorBanner((msg) => calls.push(msg));
    return { banner, calls };
  }

  it('fires the CAVEAT_MESSAGE for koryph.project.json', () => {
    const { banner, calls } = makeSpyBanner();
    banner.maybeShow('/repo/koryph/koryph.project.json');
    assert.strictEqual(calls.length, 1);
    assert.strictEqual(calls[0], CAVEAT_MESSAGE);
  });

  it('does NOT fire for non-config files', () => {
    const { banner, calls } = makeSpyBanner();
    banner.maybeShow('/repo/koryph/package.json');
    banner.maybeShow('/repo/koryph/go.sum');
    banner.maybeShow('/repo/koryph/koryph.project.json.bak');
    assert.strictEqual(calls.length, 0);
  });

  it('deduplicates: same key does not trigger twice', () => {
    const { banner, calls } = makeSpyBanner();
    const key = 'file:///repo/koryph/koryph.project.json';
    banner.maybeShow('/repo/koryph/koryph.project.json', key);
    banner.maybeShow('/repo/koryph/koryph.project.json', key);
    banner.maybeShow('/repo/koryph/koryph.project.json', key);
    assert.strictEqual(calls.length, 1);
  });

  it('fires independently for different keys (multiple open files)', () => {
    const { banner, calls } = makeSpyBanner();
    banner.maybeShow('/a/koryph.project.json', 'file:///a/koryph.project.json');
    banner.maybeShow('/b/koryph.project.json', 'file:///b/koryph.project.json');
    assert.strictEqual(calls.length, 2);
  });

  it('wasShown returns correct state', () => {
    const { banner } = makeSpyBanner();
    const key = 'file:///repo/koryph.project.json';
    assert.strictEqual(banner.wasShown(key), false);
    banner.maybeShow('/repo/koryph.project.json', key);
    assert.strictEqual(banner.wasShown(key), true);
  });

  it('reset allows re-firing', () => {
    const { banner, calls } = makeSpyBanner();
    const key = 'file:///repo/koryph.project.json';
    banner.maybeShow('/repo/koryph.project.json', key);
    assert.strictEqual(calls.length, 1);

    banner.reset(key);
    banner.maybeShow('/repo/koryph.project.json', key);
    assert.strictEqual(calls.length, 2);
  });

  it('forceShow re-fires even when already shown', () => {
    const { banner, calls } = makeSpyBanner();
    const key = 'file:///repo/koryph.project.json';
    banner.maybeShow('/repo/koryph.project.json', key);
    assert.strictEqual(calls.length, 1);

    banner.forceShow('/repo/koryph.project.json', key);
    assert.strictEqual(calls.length, 2);
  });

  it('forceShow does NOT fire for non-config files', () => {
    const { banner, calls } = makeSpyBanner();
    banner.forceShow('/repo/not-a-config.json');
    assert.strictEqual(calls.length, 0);
  });

  it('defaults the dedup key to filePath when key is omitted', () => {
    const { banner, calls } = makeSpyBanner();
    const fp = '/repo/koryph.project.json';
    banner.maybeShow(fp);
    banner.maybeShow(fp); // same path, no key argument
    assert.strictEqual(calls.length, 1, 'should dedup by path when key omitted');
  });
});

// ---------------------------------------------------------------------------
// CAVEAT_MESSAGE and REGISTRY_CAVEAT content checks
// ---------------------------------------------------------------------------

describe('banner constants', () => {
  it('CAVEAT_MESSAGE mentions koryph run and run start', () => {
    assert.match(CAVEAT_MESSAGE, /koryph run/);
    assert.match(CAVEAT_MESSAGE, /run start/);
  });

  it('REGISTRY_CAVEAT mentions koryph project CLI and registry path', () => {
    assert.match(REGISTRY_CAVEAT, /koryph project/);
    assert.match(REGISTRY_CAVEAT, /registry\.d/);
  });
});

// ---------------------------------------------------------------------------
// configFileLabel
// ---------------------------------------------------------------------------

describe('configFileLabel', () => {
  it('returns parent-dir / filename', () => {
    assert.strictEqual(
      configFileLabel('/repo/koryph/koryph.project.json'),
      'koryph/koryph.project.json',
    );
  });

  it('handles root-level paths gracefully', () => {
    const result = configFileLabel('/koryph.project.json');
    assert.ok(result.endsWith(CONFIG_FILENAME));
  });
});

// ---------------------------------------------------------------------------
// Schema binding contract: the bundled schema exists and is parseable
// ---------------------------------------------------------------------------

describe('jsonValidation schema binding', () => {
  // Resolve the extension root: compiled output is under out/test/suite/,
  // so 3 levels up from __dirname gives ide/vscode/.
  const extRoot = path.resolve(__dirname, '..', '..', '..');

  const mediaSchema = path.join(extRoot, 'media', 'koryph.project.schema.json');
  const pkgJson = path.join(extRoot, 'package.json');

  it('media/koryph.project.schema.json exists and is valid JSON', () => {
    assert.ok(
      fs.existsSync(mediaSchema),
      `Expected schema at ${mediaSchema} — run: cp docs/schema/koryph.project.schema.json ide/vscode/media/`,
    );
    const raw = fs.readFileSync(mediaSchema, 'utf8');
    let parsed: unknown;
    assert.doesNotThrow(() => { parsed = JSON.parse(raw); }, 'schema must be valid JSON');
    assert.ok(
      typeof (parsed as { $schema?: unknown }).$schema === 'string',
      'schema must have a $schema field',
    );
  });

  it('package.json jsonValidation contributes the media schema for koryph.project.json', () => {
    const pkg = JSON.parse(fs.readFileSync(pkgJson, 'utf8')) as {
      contributes?: { jsonValidation?: Array<{ fileMatch: string; url: string }> };
    };
    const entries = pkg.contributes?.jsonValidation ?? [];
    const entry = entries.find((e) => e.fileMatch === CONFIG_FILENAME);
    assert.ok(
      entry !== undefined,
      `package.json jsonValidation must have an entry with fileMatch: "${CONFIG_FILENAME}"`,
    );
    assert.ok(
      entry.url.includes('koryph.project.schema.json'),
      `jsonValidation url "${entry.url}" must reference koryph.project.schema.json`,
    );
    // The URL must be a relative extension-internal path (not https://).
    assert.ok(
      entry.url.startsWith('./'),
      `jsonValidation url must be a relative path starting with "./" — got: ${entry.url}`,
    );
  });

  it('schema $id matches the well-known koryph schema URL', () => {
    const raw = fs.readFileSync(mediaSchema, 'utf8');
    const schema = JSON.parse(raw) as { $id?: string };
    assert.strictEqual(
      schema.$id,
      'https://koryph.dev/schemas/config',
      'schema $id must be https://koryph.dev/schemas/config',
    );
  });

  it('schema title is "koryph.project.json"', () => {
    const raw = fs.readFileSync(mediaSchema, 'utf8');
    const schema = JSON.parse(raw) as { title?: string };
    assert.strictEqual(schema.title, 'koryph.project.json');
  });

  it('schema requires the mandatory fields', () => {
    const raw = fs.readFileSync(mediaSchema, 'utf8');
    const schema = JSON.parse(raw) as { required?: string[] };
    const required = schema.required ?? [];
    for (const field of [
      'schema_version',
      'project_id',
      'work_source',
      'gate',
      'merge_policy',
      'risk_tier_default',
    ]) {
      assert.ok(required.includes(field), `schema must require field "${field}"`);
    }
  });
});
