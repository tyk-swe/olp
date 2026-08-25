// @vitest-environment jsdom
// Runes only observe changes when Svelte compiles for the client, which the
// default node environment does not select.
import { flushSync, mount, unmount } from 'svelte';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { Capability, FixedRole } from './authorization';
import { authLifecycle } from './lifecycle';
import type { AuthenticatedSession } from './state';
import RoleProbe from './test/RoleProbe.svelte';

const principalId = '01980000-0000-7000-8000-000000000001';
let host: HTMLElement;

function sessionFor(role: FixedRole): AuthenticatedSession {
  return {
    user: {
      id: principalId,
      email: `${role}@example.com`,
      display_name: 'Test Principal',
      role
    },
    csrf_token: 'csrf-role-test'
  };
}

function render(capability: Capability) {
  const component = mount(RoleProbe, { target: host, props: { capability } });
  flushSync();
  return component;
}

function shown(testId: string) {
  return host.querySelector(`[data-testid="${testId}"]`)?.textContent;
}

describe('reactive principal capabilities', () => {
  beforeEach(async () => {
    host = document.createElement('div');
    document.body.append(host);
    await authLifecycle.principalInvalidated();
  });

  afterEach(() => {
    host.remove();
  });

  it('grants nothing while no principal is established', () => {
    const component = render('operations.read');

    expect(shown('role')).toBe('none');
    expect(shown('email')).toBe('none');
    expect(shown('capability')).toBe('denied');

    void unmount(component);
  });

  it('reads the capabilities of the established principal', () => {
    authLifecycle.establishSession(sessionFor('operator'));
    const component = render('providers.manage');

    expect(shown('role')).toBe('operator');
    expect(shown('email')).toBe('operator@example.com');
    expect(shown('capability')).toBe('granted');

    void unmount(component);
  });

  it('follows a role change instead of capturing the role once', () => {
    authLifecycle.establishSession(sessionFor('owner'));
    const component = render('users.manage');
    expect(shown('capability')).toBe('granted');

    // An owner can demote themselves, and another tab can sign in as somebody
    // else; write controls must disappear without remounting the component.
    authLifecycle.establishSession(sessionFor('viewer'));
    flushSync();

    expect(shown('role')).toBe('viewer');
    expect(shown('email')).toBe('viewer@example.com');
    expect(shown('capability')).toBe('denied');

    void unmount(component);
  });

  it('re-grants a capability when the principal is promoted', () => {
    authLifecycle.establishSession(sessionFor('developer'));
    const component = render('routes.manage');
    expect(shown('capability')).toBe('denied');

    authLifecycle.establishSession(sessionFor('operator'));
    flushSync();

    expect(shown('capability')).toBe('granted');

    void unmount(component);
  });

  it('drops its subscription when the component is destroyed', async () => {
    authLifecycle.establishSession(sessionFor('viewer'));
    const component = render('users.manage');
    expect(shown('role')).toBe('viewer');

    await unmount(component, { outro: false });
    authLifecycle.establishSession(sessionFor('owner'));
    flushSync();

    expect(host.querySelector('[data-testid="role"]')).toBeNull();
  });
});
