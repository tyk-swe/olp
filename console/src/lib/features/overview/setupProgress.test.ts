import { describe, expect, it } from 'vitest';
import { setupProgress } from './setupProgress';
import { operationsFor } from '$lib/features/access/session/test/grants';

function grant(role: 'owner' | 'operator' | 'developer' | 'viewer') {
  return { operations: operationsFor(role), access_scope: 'global' as const };
}

const empty = {
  loading: false,
  failed: false,
  activeProvider: false,
  enabledModels: false,
  activeRoute: false,
  apiKey: false,
  grant: grant('owner')
};
const ready = {
  ...empty,
  activeProvider: true,
  enabledModels: true,
  activeRoute: true,
  apiKey: true
};

describe('setup guidance', () => {
  it('advances from connection to model review, routing, and key creation', () => {
    expect(setupProgress(empty).nextStep?.href).toBe('/providers/new');
    expect(
      setupProgress({ ...empty, activeProvider: true }).nextStep?.href
    ).toBe('/models');
    expect(
      setupProgress({ ...empty, activeProvider: true, enabledModels: true })
        .nextStep?.href
    ).toBe('/routes/new');
    expect(setupProgress({ ...ready, apiKey: false }).nextStep?.href).toBe(
      '/api-keys/new'
    );
    expect(setupProgress(ready)).toMatchObject({
      complete: true,
      completeCount: 5,
      nextStep: undefined
    });
  });

  it.each([
    { loading: true, failed: false },
    { loading: false, failed: true },
    { loading: true, failed: true }
  ])(
    'does not infer completion or a next action from unavailable data: %j',
    (status) => {
      const result = setupProgress({ ...ready, ...status });
      expect(result).toMatchObject({
        settled: false,
        complete: false,
        completeCount: 0,
        nextStep: undefined
      });
      expect(result.steps.some((step) => step.current || step.complete)).toBe(
        false
      );
    }
  );

  it('offers only permitted actions and retains viewing links for blocked steps', () => {
    const developer = setupProgress({ ...empty, grant: grant('developer') });
    expect(developer.nextStep?.href).toBe('/api-keys/new');
    expect(developer.steps[1]).toMatchObject({
      href: '/providers',
      allowed: false,
      current: true
    });
    expect(developer.steps[1].permissionNote).toContain('owner or operator');
    const viewer = setupProgress({ ...empty, grant: grant('viewer') });
    expect(viewer.nextStep).toBeUndefined();
    expect(viewer.steps[4].href).toBe('/api-keys');
  });

  it('recomputes permissions when a principal is demoted', () => {
    expect(setupProgress({ ...ready, apiKey: false }).nextStep?.href).toBe(
      '/api-keys/new'
    );
    expect(
      setupProgress({ ...ready, apiKey: false, grant: grant('viewer') })
        .nextStep
    ).toBeUndefined();
  });
});

it('links completed steps to their inventory without asking for more setup', () => {
  const progress = setupProgress({
    ...ready,
    apiKey: false,
    grant: grant('viewer')
  });
  expect(progress.steps[1]).toMatchObject({
    href: '/providers',
    complete: true,
    permissionNote: ''
  });
  expect(progress.steps[3]).toMatchObject({
    href: '/routes',
    complete: true,
    permissionNote: ''
  });
});
