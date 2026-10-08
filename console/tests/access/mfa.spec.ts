import { createHmac } from 'node:crypto';
import AxeBuilder from '@axe-core/playwright';
import { expect, test } from '../playwright';
import { gatewayOwner, signInGatewayOwner } from '../gateway/signIn';

function totp(secret: string) {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';
  const bits = [...secret.replace(/=+$/, '')]
    .map((c) => alphabet.indexOf(c).toString(2).padStart(5, '0'))
    .join('');
  const key = Buffer.from(
    (bits.match(/.{8}/g) ?? []).map((byte) => Number.parseInt(byte, 2))
  );
  const counter = Buffer.alloc(8);
  counter.writeBigUInt64BE(BigInt(Math.floor(Date.now() / 30000)));
  const digest = createHmac('sha1', key).update(counter).digest();
  const offset = digest[19]! & 15;
  return ((digest.readUInt32BE(offset) & 0x7fffffff) % 1000000)
    .toString()
    .padStart(6, '0');
}
test('enrolls local factors and completes password sign-in only after WebAuthn verification', async ({
  page
}, info) => {
  await signInGatewayOwner(page);
  const cdp = await page.context().newCDPSession(page);
  await cdp.send('WebAuthn.enable');
  await cdp.send('WebAuthn.addVirtualAuthenticator', {
    options: {
      protocol: 'ctap2',
      transport: 'internal',
      hasResidentKey: true,
      hasUserVerification: true,
      isUserVerified: true,
      automaticPresenceSimulation: true
    }
  });
  await page.goto('/settings/profile');
  const panel = page.locator('section[aria-labelledby="mfa-heading"]');
  await panel
    .getByLabel('Current password for enrollment')
    .fill(gatewayOwner.password);
  await panel.getByLabel('Authenticator name').fill('Personal authenticator');
  await panel
    .getByRole('button', { name: 'Enroll authenticator', exact: true })
    .click();
  let dialog = page.getByRole('dialog', {
    name: 'Multi-factor authentication'
  });
  const seed = await dialog.getByLabel('Manual setup key').inputValue();
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await dialog
    .getByLabel('Authentication code', { exact: true })
    .fill(totp(seed));
  await dialog.getByRole('button', { name: 'Verify', exact: true }).click();
  const codes = (
    await dialog.getByLabel('Recovery codes', { exact: true }).inputValue()
  ).split('\n');
  expect(codes).toHaveLength(10);
  await dialog
    .getByRole('button', { name: 'I saved my recovery codes' })
    .click();
  await expect(
    panel.getByText('Personal authenticator', { exact: true })
  ).toBeVisible();
  await panel.getByLabel('Authenticator type').selectOption('webauthn');
  await panel.getByLabel('Authenticator name').fill('Browser security key');
  await panel
    .getByRole('button', { name: 'Enroll authenticator', exact: true })
    .click();
  dialog = page.getByRole('dialog', { name: 'Multi-factor authentication' });
  await dialog.getByLabel('Verification method').selectOption('recovery');
  await dialog.getByLabel('Recovery code', { exact: true }).fill(codes[0]!);
  await dialog.getByRole('button', { name: 'Verify', exact: true }).click();
  await dialog
    .getByRole('button', { name: 'Use security key', exact: true })
    .click();
  await expect(
    panel.getByText('Browser security key', { exact: true })
  ).toBeVisible();
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.evaluate(async () => {
    (document.activeElement as HTMLElement | null)?.blur();
    window.scrollTo(0, 0);
    await new Promise<void>((resolve) =>
      requestAnimationFrame(() => requestAnimationFrame(() => resolve()))
    );
  });
  await page.screenshot({
    path: info.outputPath('mfa-factors.png'),
    fullPage: true
  });
  await page.evaluate(async () => {
    const session = await (await fetch('/api/v1/sessions/current')).json();
    await fetch('/api/v1/sessions/current', {
      method: 'DELETE',
      headers: { 'X-CSRF-Token': session.csrf_token }
    });
  });
  await page.goto('/login');
  await page.getByLabel('Email', { exact: true }).fill('owner@example.com');
  await page
    .getByLabel('Password', { exact: true })
    .fill(gatewayOwner.password);
  await page.getByRole('button', { name: 'Sign in', exact: true }).click();
  dialog = page.getByRole('dialog', { name: 'Multi-factor authentication' });
  await expect(dialog).toBeVisible();
  expect(
    await page.evaluate(
      async () => (await fetch('/api/v1/sessions/current')).status
    )
  ).toBe(401);
  await dialog.getByLabel('Verification method').selectOption('webauthn');
  await dialog
    .getByRole('button', { name: 'Use security key', exact: true })
    .click();
  await expect(page).not.toHaveURL(/\/login/);
  expect(
    await page.evaluate(
      async () => (await fetch('/api/v1/sessions/current')).status
    )
  ).toBe(200);
});
