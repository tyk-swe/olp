import { expect, it, onTestFinished, vi } from 'vitest';
import { flushSync, getAllContexts, mount, unmount } from 'svelte';
import { json, serve } from '#lib/management/test/transport.js';
import StateHarness from '../../../../tests/unit/support/StateHarness.svelte';
import GuardrailPreview from './GuardrailPreview.svelte';
import type { PreviewEntry } from './api';
import { eventually } from '../../../../tests/unit/support/eventually.js';

const notice =
  'This preview shows only guardrails you can view. Full-policy compatibility is not shown.';
const projectEntry = {
  policy_id: 'project-policy',
  policy_revision_id: 'project-revision',
  guardrail_id: 'project-check',
  revision_id: 'project-check-revision',
  scope: 'project',
  phase: 'output',
  mode: 'enforce',
  notes: ['streaming_unavailable']
} satisfies PreviewEntry;
const keyEntry = {
  ...projectEntry,
  policy_id: 'key-policy',
  scope: 'api-key',
  phase: 'input',
  mode: 'monitor',
  notes: []
} satisfies PreviewEntry;

it.each([true, false])(
  'keeps the scoped preview identical with zero, one or several hidden policies (visible entries: %s)',
  async (withEntries) => {
    const entries = withEntries ? [projectEntry, keyEntry] : [];
    const view = await mountPreview({ partial_visibility: true, entries });
    await view.inspect('zero-hidden');
    const baseline = view.result().textContent;
    expect(baseline).toContain(notice);
    expect(baseline).not.toContain('No static guardrail conflicts');
    expect(baseline).not.toContain('chain has a conflict');
    if (withEntries) {
      const rows = [...view.result().querySelectorAll('tbody tr')].map((row) =>
        [...row.querySelectorAll('td')].map((cell) => cell.textContent)
      );
      expect(rows).toEqual([
        [
          '1',
          expect.stringContaining('project-policy'),
          expect.stringContaining('project-check'),
          'project',
          'output',
          'Enforce',
          'This guardrail cannot inspect the selected stream.'
        ],
        [
          '2',
          expect.stringContaining('key-policy'),
          expect.stringContaining('project-check'),
          'api-key',
          'input',
          'Monitor',
          'None'
        ]
      ]);
    } else {
      expect(baseline).toContain('No visible guardrails for this selection.');
    }
    await view.inspect('one-hidden');
    expect(view.result().textContent).toEqual(baseline);
    await view.inspect('several-hidden');
    expect(view.result().textContent).toEqual(baseline);
  }
);

it('shows the full serving order and compatibility only to an installation-wide reader', async () => {
  const view = await mountPreview({
    partial_visibility: false,
    static_compatible: false,
    entries: [
      {
        ...projectEntry,
        policy_id: 'installation-policy',
        scope: 'installation'
      },
      projectEntry,
      keyEntry
    ]
  });
  await view.inspect('several-hidden');
  expect(view.result().textContent).not.toContain(notice);
  expect(view.result().textContent).toContain(
    'The guardrail chain has a conflict on the selected surface.'
  );
  expect(view.result().textContent).toContain(
    'A newer release has been published.'
  );
  expect(
    [
      ...view
        .result()
        .querySelectorAll('tbody tr td:nth-child(2) code:first-child')
    ].map((cell) => cell.textContent)
  ).toEqual(['installation-policy', 'project-policy', 'key-policy']);
});

it.each([
  { operation: 'generation', surface: 'gemini-interactions' },
  { operation: 'speech', surface: 'openai' }
])(
  'requests the streaming preview for $operation on $surface',
  async (selection) => {
    const view = await mountPreview(
      { partial_visibility: false, static_compatible: false, entries: [] },
      selection
    );
    await view.inspect('zero-hidden');
  }
);

async function mountPreview(
  payload: {
    partial_visibility: boolean;
    static_compatible?: boolean;
    entries: PreviewEntry[];
  },
  selection = { operation: 'generation', surface: 'openai-chat' }
) {
  const fixtures = ['zero-hidden', 'one-hidden', 'several-hidden'];
  const requested = vi.fn<(route: string) => void>();
  serve(
    (request) => {
      const url = new URL(request.url);
      switch (url.pathname) {
        case '/api/v1/routes':
          return json({
            items: fixtures.map((id) => ({
              id,
              slug: id,
              project_id: 'project',
              state: 'active',
              latest_revision: {
                operations: [selection.operation],
                fidelity: { mode: 'transformed' }
              }
            }))
          });
        case '/api/v1/api-keys':
          return json({
            items: [
              { id: 'key', name: 'Key', project_id: 'project' },
              {
                id: 'other-key',
                name: 'Other Key',
                project_id: 'other-project'
              }
            ]
          });
        case '/api/v1/operation-dialects':
          return json({ items: [] });
        case '/api/v1/guardrails/preview':
          expect(request.method).toBe('GET');
          expect(request.body).toBeNull();
          expect(Object.fromEntries(url.searchParams)).toEqual({
            route_id: expect.stringMatching(/hidden$/),
            api_key_id: 'key',
            ...selection,
            mode: 'streaming',
            retains_output: 'false'
          });
          requested(url.searchParams.get('route_id') ?? '');
          return json({
            release_id: 'release-id',
            release_sequence: 41,
            stale: true,
            ...payload
          });
        default:
          return json({ title: 'unexpected request' }, 404);
      }
    },
    ['read']
  );
  const target = document.createElement('div');
  document.body.append(target);
  const setup = vi.fn(() =>
    mount(GuardrailPreview, { target, context: getAllContexts() })
  );
  const harness = mount(StateHarness, { target, props: { setup } });
  onTestFinished(async () => {
    await unmount(setup.mock.results[0].value);
    await unmount(harness);
    target.remove();
    vi.unstubAllGlobals();
  });
  flushSync();
  const result = () => {
    const region = target.querySelector('[aria-label="Preview result"]');
    if (!region) throw new Error('Missing preview result');
    return region;
  };
  await eventually(() => expect(target.querySelector('form')).not.toBeNull());
  function select(index: number, value: string) {
    const field = target.querySelectorAll('select')[index];
    if (!field) throw new Error(`Missing selection ${index}`);
    field.value = value;
    field.dispatchEvent(new Event('input', { bubbles: true }));
    field.dispatchEvent(new Event('change', { bubbles: true }));
    flushSync();
  }
  return {
    result,
    inspect: async (route: string) => {
      select(0, route);
      expect(target.querySelector('option[value="other-key"]')).toBeNull();
      select(1, 'key');
      select(2, selection.operation);
      select(3, selection.surface);
      select(4, 'streaming');
      const button = target.querySelector('button');
      expect(button?.disabled).toBe(false);
      button?.click();
      flushSync();
      await eventually(() => {
        expect(requested).toHaveBeenLastCalledWith(route);
        expect(result().textContent).toContain('Serving release 41');
      });
    }
  };
}
