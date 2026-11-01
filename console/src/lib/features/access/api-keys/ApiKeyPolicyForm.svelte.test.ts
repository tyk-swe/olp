import { flushSync, mount, unmount } from 'svelte';
import { expect, it, onTestFinished, vi } from 'vitest';
import type { ApiKey } from './api';
import type { ApiKeyPolicyInput } from './apiKeyPolicy';
import ApiKeyPolicyForm from './ApiKeyPolicyForm.svelte';

vi.mock('@tanstack/svelte-query', () => ({
  createQuery: () => ({ data: [], isPending: false, isError: false })
}));
vi.mock('$lib/features/access/session/serviceCapabilities.svelte', () => ({
  useServiceCapabilities: () => ({
    gatewayAvailable: true,
    limitsEnforced: true
  })
}));
vi.mock('$lib/features/access/session/useRole.svelte', () => ({
  useRole: () => ({ user: { access_scope: 'all' } })
}));
vi.mock('$lib/forms/unsavedChanges', () => ({ guardUnsavedChanges: () => {} }));

function field(label: string): HTMLInputElement {
  const node = [...document.querySelectorAll('label')].find(
    (candidate) => candidate.textContent?.trim() === label
  );
  const input = node?.htmlFor && document.getElementById(node.htmlFor);
  expect(input, `Missing ${label}`).toBeInstanceOf(HTMLInputElement);
  return input as HTMLInputElement;
}

function enter(label: string, value: string) {
  const input = field(label);
  input.value = value;
  input.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
}

it('saves workflow limits and shows them again when the key is edited', async () => {
  const target = document.createElement('div');
  document.body.append(target);
  const submission = Promise.withResolvers<ApiKeyPolicyInput>();
  const submit = async (input: ApiKeyPolicyInput) => {
    submission.resolve(input);
    return true;
  };
  const created = mount(ApiKeyPolicyForm, {
    target,
    props: {
      editing: null,
      busy: '',
      submitError: '',
      canManage: true,
      onSubmit: submit,
      onCancel: () => {},
      onClearError: () => {}
    }
  });
  onTestFinished(() => target.remove());
  flushSync();

  enter('Key name', 'workflow key');
  enter('Maximum model iterations', '4');
  enter('Maximum tool calls', '8');
  enter('Maximum run duration in seconds', '60');
  enter('Maximum concurrent runs per key', '1');
  enter('Maximum run cost', '0.75');
  target
    .querySelector('form')
    ?.dispatchEvent(
      new SubmitEvent('submit', { bubbles: true, cancelable: true })
    );
  const saved = await submission.promise;
  expect(saved.run_limits).toEqual({
    max_iterations: 4,
    max_tool_calls: 8,
    max_duration_seconds: 60,
    max_concurrent_runs: 1,
    max_run_cost: '0.75'
  });
  await unmount(created);

  const budgetWindow = {
    accrued: '0',
    limit: null,
    window_ends_at: '2030-01-01'
  };
  const key: ApiKey = {
    id: 'key',
    lookup_id: 'key',
    name: saved.name,
    project_id: null,
    project_name: null,
    budget_group_id: null,
    created_by: 'member',
    created_by_email: 'member@example.test',
    scopes: ['inference'],
    allowed_routes: [],
    allow_provider_state: false,
    budget: {
      daily: budgetWindow,
      monthly: budgetWindow,
      unpriced_attempts: 0
    },
    etag: 'etag',
    created_at: '2026-01-01',
    allowed_attribution_keys: [],
    response_metadata: false,
    regional_limits: {},
    run_limits: saved.run_limits
  };
  const edited = mount(ApiKeyPolicyForm, {
    target,
    props: {
      editing: key,
      busy: '',
      submitError: '',
      canManage: true,
      onSubmit: submit,
      onCancel: () => {},
      onClearError: () => {}
    }
  });
  onTestFinished(() => unmount(edited));
  flushSync();
  expect(field('Maximum model iterations').value).toBe('4');
  expect(field('Maximum tool calls').value).toBe('8');
  expect(field('Maximum run duration in seconds').value).toBe('60');
  expect(field('Maximum concurrent runs per key').value).toBe('1');
  expect(field('Maximum run cost').value).toBe('0.75');
});
