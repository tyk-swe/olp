import { execFileSync } from 'node:child_process';
import { mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import AxeBuilder from '@axe-core/playwright';
import { expect, test } from '../playwright';
import { signInGatewayOwner as signIn } from '../gateway/signIn';

let module = '';

// The journey installs the reference plugin, built from the SDK exactly as a
// plugin author would build it.
test.beforeAll(() => {
  module = join(mkdtempSync(join(tmpdir(), 'olp-plugin-')), 'reference.wasm');
  execFileSync(
    'go',
    ['build', '-buildmode=c-shared', '-o', module, './sdk/plugin/reference'],
    {
      cwd: fileURLToPath(new URL('../../..', import.meta.url)),
      env: { ...process.env, GOOS: 'wasip1', GOARCH: 'wasm', CGO_ENABLED: '0' },
      stdio: 'inherit'
    }
  );
});

test('an owner installs, approves and uninstalls a provider plugin', async ({
  page
}, info) => {
  await signIn(page);
  await page.goto('/plugins');
  await expect(
    page.getByRole('heading', { name: 'Plugins', exact: true })
  ).toBeVisible();
  // Other journeys may leave their own builds of the plugin installed.
  const plugin = page.getByRole('article', { name: 'reference 0.1.0' });
  await expect(plugin).toHaveCount(0);

  await page.getByLabel('Plugin module (.wasm)').setInputFiles(module);
  await page.getByRole('button', { name: 'Install plugin' }).click();
  await expect(page.getByRole('status')).toContainText(
    'Installed reference 0.1.0.'
  );
  await expect(plugin.locator('.badge')).toHaveText('Pending approval');
  // One profile places the credential, one signs requests with it, one
  // places the provider's workspace option in its address, and one
  // authenticates with a grant the plugin enrolls, whose facts name the
  // address.
  for (const [id, address, authentication] of [
    ['reference-chat', 'https://api.example.com/v1', 'Static credential'],
    [
      'reference-signed-chat',
      'https://api.example.com/v1',
      'Static credential'
    ],
    [
      'reference-workspace-chat',
      'https://api.example.com/v1/workspaces/{options.workspace}',
      'Static credential'
    ],
    [
      'reference-grant-chat',
      '{grant.api_base}',
      'Grant, enrolled by the plugin'
    ]
  ] as const) {
    const profile = plugin.getByRole('row', { name: new RegExp(`^${id} `) });
    await expect(
      profile.getByRole('cell', { name: 'openai-chat', exact: true })
    ).toBeVisible();
    await expect(
      profile.getByRole('cell', { name: authentication, exact: true })
    ).toBeVisible();
    await expect(
      profile.getByRole('cell', { name: address, exact: true })
    ).toBeVisible();
  }
  await expect(
    plugin
      .getByRole('region', { name: 'Declared origins' })
      .getByRole('listitem')
  ).toHaveText(['https://api.example.com', 'https://login.example.com']);
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.screenshot({
    path: info.outputPath('plugins-pending.png'),
    fullPage: true
  });

  // Installing the same digest again changes nothing.
  await page.getByLabel('Plugin module (.wasm)').setInputFiles(module);
  await page.getByRole('button', { name: 'Install plugin' }).click();
  await expect(page.getByRole('status')).toContainText(
    'reference 0.1.0 is already installed; nothing changed.'
  );
  await expect(plugin).toHaveCount(1);

  await plugin.getByRole('button', { name: 'Review and approve' }).click();
  const approval = plugin.getByRole('region', {
    name: 'Approve these origins?'
  });
  await expect(approval.getByRole('listitem')).toHaveText([
    'https://api.example.com',
    'https://login.example.com'
  ]);
  await approval.getByRole('button', { name: 'Approve origins' }).click();
  await expect(page.getByRole('status')).toContainText(
    'Approved reference 0.1.0.'
  );
  await expect(plugin.locator('.badge')).toHaveText('Approved');
  await expect(
    plugin.getByRole('button', { name: 'Review and approve' })
  ).toHaveCount(0);

  page.once('dialog', (dialog) => dialog.accept());
  await plugin.getByRole('button', { name: 'Uninstall' }).click();
  await expect(page.getByRole('status')).toContainText(
    'Uninstalled reference 0.1.0.'
  );
  await expect(plugin).toHaveCount(0);
});
