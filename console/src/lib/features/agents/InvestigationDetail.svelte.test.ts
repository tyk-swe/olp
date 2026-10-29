import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { expect, it, onTestFinished, vi } from 'vitest';
import * as api from './api';
import InvestigationDetailProbe from './test/InvestigationDetailProbe.svelte';

vi.mock('./api', async (original) => ({
  ...(await original<typeof import('./api')>()),
  getInvestigation: vi.fn()
}));

it('links each retained evidence reference to its authorized source', async () => {
  vi.mocked(api.getInvestigation).mockResolvedValue({
    id: 'investigation',
    project_id: 'project',
    created_at: '2026-10-24T09:00:00Z',
    summary: 'Review',
    filters: {},
    evidence: [
      { kind: 'run', id: 'run' },
      { kind: 'task', id: 'task' },
      { kind: 'delegation', id: 'delegation' },
      { kind: 'approval', id: 'approval', run_id: 'associated-run' },
      { kind: 'step', id: 'step', run_id: 'associated-run' },
      { kind: 'span', id: 'span', run_id: 'associated-run' },
      { kind: 'span', id: 'expired-span' }
    ]
  });
  const host = document.createElement('div');
  document.body.append(host);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } }
  });
  const component = mount(InvestigationDetailProbe, {
    target: host,
    props: { client }
  });
  onTestFinished(async () => {
    await unmount(component);
    client.clear();
    host.remove();
  });
  await vi.waitFor(() => {
    flushSync();
    const links = Array.from(host.querySelectorAll('a'));
    const href = (id: string) =>
      links
        .find((link) => link.textContent?.trim() === id)
        ?.getAttribute('href');
    expect(href('run')).toBe('/runs/run');
    expect(href('task')).toBe('/a2a-tasks/task');
    expect(href('delegation')).toBe(
      '/delegations?project_id=project&delegation_id=delegation'
    );
    expect(href('approval')).toBe('/runs/associated-run');
    expect(href('step')).toBe('/runs/associated-run?step_id=step');
    expect(href('span')).toBe('/runs/associated-run?span_id=span');
    expect(href('expired-span')).toBeUndefined();
    expect(host.textContent).toContain('expired-span');
  });
});
