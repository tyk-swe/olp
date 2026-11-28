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
import InvitationsPanel from './InvitationsPanel.svelte';

const capabilities = {
  local_login_enabled: true,
  notifications_active: false,
  payload_capture_active: false,
  oidc_login_enabled: false
};

it('reports a refused invitation at the input that holds it', async () => {
  const invited = vi.fn();
  serve(
    async (request) => {
      if (request.url.endsWith('/api/v1/auth/capabilities'))
        return json(capabilities);
      if (request.method === 'GET')
        return json({ items: [], next_cursor: null });
      invited(await request.json());
      return json(
        {
          title: 'Invalid request',
          status: 422,
          errors: {
            email: [{ code: 'invalid', message: 'Use a work address.' }]
          }
        },
        422
      );
    },
    ['access', 'access_read']
  );
  const { target } = mountWithState(InvitationsPanel, {}, { attach: true });
  const email = await eventually(() => {
    const input = findControl(target, 'Email address');
    expect(input.disabled).toBe(false);
    return input;
  });

  setFieldValue(email, 'olive');
  submitForm(target);
  await eventually(() => expect(document.activeElement).toBe(email));
  expect(reportedError(email)).toBe('Enter a valid email address.');
  expect(invited).not.toHaveBeenCalled();

  setFieldValue(email, ' owner@example.test ');
  submitForm(target);
  await eventually(() =>
    expect(reportedError(email)).toBe('Use a work address.')
  );
  expect(target.querySelector('[role="alert"]')).toBeNull();
  expect(invited).toHaveBeenCalledExactlyOnceWith({
    email: 'owner@example.test',
    role: 'developer',
    expires_in_hours: 168
  });
});
