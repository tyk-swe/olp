// @vitest-environment jsdom
// The dialog only exists once mounted: its modal lifecycle, focus move, and
// inline error all run in component code rather than in a pure helper.
import { flushSync, mount, unmount } from 'svelte';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import ReauthenticateProbe from './test/ReauthenticateProbe.svelte';

const correctPassword = 'correct horse battery staple';
let host: HTMLElement;

// jsdom implements <dialog> markup but not its modal methods.
const dialogPrototype = HTMLDialogElement.prototype as unknown as Record<
  string,
  unknown
>;

beforeEach(() => {
  dialogPrototype.showModal = function showModal(this: HTMLDialogElement) {
    this.open = true;
  };
  dialogPrototype.close = function close(this: HTMLDialogElement) {
    this.open = false;
  };
  host = document.createElement('div');
  document.body.append(host);
});

afterEach(() => {
  host.remove();
  delete dialogPrototype.showModal;
  delete dialogPrototype.close;
});

function render(accepts = correctPassword) {
  const component = mount(ReauthenticateProbe, { target: host, props: { accepts } });
  flushSync();
  return component;
}

function shown(testId: string) {
  return host.querySelector(`[data-testid="${testId}"]`)?.textContent;
}

function passwordField() {
  const field = host.querySelector<HTMLInputElement>('#reauth-password');
  if (!field) throw new Error('The reauthentication dialog is not mounted.');
  return field;
}

async function submitPassword(password: string) {
  const field = passwordField();
  field.value = password;
  field.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
  host
    .querySelector('form')
    ?.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  // The confirmation handler resolves on a microtask before it reports back.
  await Promise.resolve();
  await Promise.resolve();
  flushSync();
}

function alertText() {
  return host.querySelector('[role="alert"]')?.textContent?.trim();
}

describe('reauthentication dialog', () => {
  it('opens modally and focuses the password field', () => {
    const component = render();

    expect(host.querySelector('dialog')?.open).toBe(true);
    expect(document.activeElement).toBe(passwordField());
    expect(alertText()).toBeUndefined();

    void unmount(component);
  });

  it('rejects an empty password without asking the caller to verify it', async () => {
    const component = render();

    await submitPassword('');

    expect(alertText()).toBe('Enter your current password.');
    expect(shown('attempts')).toBe('0');
    expect(shown('state')).toBe('open');
    expect(host.querySelector('dialog')).not.toBeNull();

    void unmount(component);
  });

  it('keeps the dialog open with an inline error after a wrong password', async () => {
    const component = render();

    await submitPassword('not the password');

    expect(shown('attempts')).toBe('1');
    expect(alertText()).toBe('That password is incorrect.');
    expect(shown('state')).toBe('open');
    // The field is cleared for the retry but the dialog never unmounts.
    expect(passwordField().value).toBe('');

    void unmount(component);
  });

  it('closes once the caller accepts the password', async () => {
    const component = render();

    await submitPassword(correctPassword);

    expect(shown('attempts')).toBe('1');
    expect(shown('state')).toBe('closed');
    expect(host.querySelector('dialog')).toBeNull();

    void unmount(component);
  });
});
