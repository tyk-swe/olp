import { expect, it, vi } from 'vitest';
import { json, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import {
  findControl,
  mountWithState,
  reportedError,
  setFieldValue,
  submitForm
} from '#test/mount.js';
import LoginPage from './LoginPage.svelte';

vi.mock('$app/state', () => ({
  page: { url: new URL('http://localhost/login') }
}));

it('reports a missing credential at the input that holds it', async () => {
  const logins = vi.fn();
  serve((request) => {
    if (new URL(request.url).pathname !== '/api/v1/auth/capabilities')
      logins(request.method);
    return json({
      local_login_enabled: true,
      notifications_active: false,
      payload_capture_active: false,
      oidc_login_enabled: false
    });
  });
  const { target } = mountWithState(LoginPage, {}, { attach: true });
  const email = await eventually(() => findControl(target, 'Email'));
  const password = findControl(target, 'Password');

  submitForm(target);
  await eventually(() => expect(document.activeElement).toBe(email));
  expect(reportedError(email)).toBe('Enter your email.');
  expect(reportedError(password)).toBe('Enter your password.');

  setFieldValue(email, 'owner@example.test');
  submitForm(target);
  await eventually(() => expect(document.activeElement).toBe(password));
  expect(reportedError(email)).toBeUndefined();
  expect(reportedError(password)).toBe('Enter your password.');
  expect(target.querySelector('[role="alert"]')).toBeNull();
  expect(logins).not.toHaveBeenCalledWith('POST');
});
