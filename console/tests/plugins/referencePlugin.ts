import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdtempSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { expect, type Locator, type Page } from '../playwright';

/** Builds the reference plugin from the SDK, as a plugin author would, linking
 * each of `variables` into package main. */
export function buildReferencePlugin(variables: Record<string, string> = {}) {
  const module = join(
    mkdtempSync(join(tmpdir(), 'olp-plugin-')),
    'reference.wasm'
  );
  const ldflags = Object.entries(variables).map(
    ([name, value]) => `-X=main.${name}=${value}`
  );
  execFileSync(
    'go',
    [
      'build',
      '-buildmode=c-shared',
      ...(ldflags.length > 0 ? [`-ldflags=${ldflags.join(' ')}`] : []),
      '-o',
      module,
      './sdk/plugin/reference'
    ],
    {
      cwd: fileURLToPath(new URL('../../..', import.meta.url)),
      env: { ...process.env, GOOS: 'wasip1', GOARCH: 'wasm', CGO_ENABLED: '0' },
      stdio: 'inherit'
    }
  );
  const digest = createHash('sha256')
    .update(readFileSync(module))
    .digest('hex');
  return { module, digest };
}

export async function installReferencePlugin(
  page: Page,
  module: string,
  version: string
) {
  await page.goto('/plugins');
  await page.getByLabel('Plugin module (.wasm)').setInputFiles(module);
  await page.getByRole('button', { name: 'Install plugin' }).click();
  await expect(page.getByRole('status')).toContainText(
    `Installed reference ${version}.`
  );
  return page.getByRole('article', { name: `reference ${version}` });
}

export async function approveReferencePlugin(
  page: Page,
  plugin: Locator,
  version: string
) {
  await plugin.getByRole('button', { name: 'Review and approve' }).click();
  await plugin
    .getByRole('region', { name: 'Approve these origins?' })
    .getByRole('button', { name: 'Approve origins' })
    .click();
  await expect(page.getByRole('status')).toContainText(
    `Approved reference ${version}.`
  );
}
