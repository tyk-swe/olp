import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { beforeEach, expect, it, onTestFinished, vi } from 'vitest';
import * as api from './api';
import * as projects from '$lib/features/access/projects/api';
import ObservabilityProbe from './test/ObservabilityProbe.svelte';

vi.mock('./api', async (original) => ({
  ...(await original<typeof import('./api')>()),
  listExportSinks: vi.fn(),
  createExportSink: vi.fn(),
  getCaptureConfiguration: vi.fn(),
  listCapturePolicies: vi.fn(),
  listCaptureSinkOptions: vi.fn(),
  listCaptureRedactorOptions: vi.fn()
}));
vi.mock('$lib/features/access/projects/api', () => ({
  listProjectMemberships: vi.fn()
}));
vi.mock('$lib/features/access/session/useRole.svelte', () => ({
  useRole: () => ({
    user: {
      role: 'owner',
      access_scope: 'global',
      operations: ['settings', 'api_keys.manage']
    },
    can: () => true
  })
}));

const project = '00000000-0000-7000-8000-000000000001';
const streams: api.ExportSink['streams'] = ['runs', 'trace_spans'];

beforeEach(() => {
  vi.resetAllMocks();
  vi.mocked(projects.listProjectMemberships).mockResolvedValue([
    { id: project, name: 'Example project', role: 'manager' }
  ]);
  vi.mocked(api.getCaptureConfiguration).mockResolvedValue({
    enabled: false,
    etag: 'capture-etag'
  });
  vi.mocked(api.listCapturePolicies).mockResolvedValue([]);
  vi.mocked(api.listCaptureSinkOptions).mockResolvedValue([]);
  vi.mocked(api.listCaptureRedactorOptions).mockResolvedValue([]);
});

it.each([
  { scope: 'project', projectId: project },
  { scope: 'installation', projectId: null }
])('creates and reopens $scope evidence exports', async ({ projectId }) => {
  const sink: api.ExportSink = {
    export_sink_id: 'sink',
    name: 'Evidence sink',
    project_id: projectId,
    type: 'https',
    destination: 'https://collector.example.test/evidence',
    streams,
    format: 'json',
    filter: null,
    enabled: true,
    credential_configured: false,
    etag: 'etag',
    status: []
  };
  vi.mocked(api.listExportSinks)
    .mockResolvedValueOnce([])
    .mockResolvedValue([sink]);
  vi.mocked(api.createExportSink).mockImplementation(async (input) => {
    expect(input.project_id).toBe(projectId);
    expect(input.streams).toEqual(streams);
    return sink;
  });
  const host = document.createElement('div');
  document.body.append(host);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } }
  });
  const component = mount(ObservabilityProbe, {
    target: host,
    props: { client }
  });
  onTestFinished(async () => {
    await unmount(component);
    client.clear();
    host.remove();
  });

  await vi.waitFor(() => {
    expect(
      host.querySelector('option[value="' + project + '"]')
    ).not.toBeNull();
  });
  fill(host, 'Name', sink.name);
  fill(host, 'Destination', sink.destination);
  const projectSelect = host.querySelector<HTMLSelectElement>('#sink-project');
  if (!projectSelect) expect.fail('Missing project selection');
  projectSelect.value = projectId ?? '';
  projectSelect.dispatchEvent(new Event('change', { bubbles: true }));
  checkbox(host, 'Requests').click();
  checkbox(host, 'Runs (gateway observed)').click();
  checkbox(host, 'Trace spans (client asserted)').click();
  flushSync();
  button(host, 'Create sink').click();
  await vi.waitFor(() => expect(host.textContent).toContain('Sink created.'));
  await vi.waitFor(() => expect(host.textContent).toContain(sink.name));
  button(host, 'Edit').click();
  await vi.waitFor(() => {
    expect(checkbox(host, 'Runs (gateway observed)').checked).toBe(true);
    expect(checkbox(host, 'Trace spans (client asserted)').checked).toBe(true);
  });
});

function fill(host: HTMLElement, label: string, value: string) {
  const labelElement = [...host.querySelectorAll('label')].find(
    (element) => element.textContent?.trim() === label
  );
  const input =
    labelElement?.htmlFor &&
    host.querySelector<HTMLInputElement>(`#${labelElement.htmlFor}`);
  if (!input) expect.fail(`Missing ${label} input`);
  input.value = value;
  input.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
}

function checkbox(host: HTMLElement, label: string): HTMLInputElement {
  const input = [...host.querySelectorAll('label')]
    .find((element) => element.textContent?.trim() === label)
    ?.querySelector('input[type="checkbox"]');
  if (!(input instanceof HTMLInputElement))
    expect.fail(`Missing ${label} checkbox`);
  return input;
}

function button(host: HTMLElement, label: string): HTMLButtonElement {
  const result = [...host.querySelectorAll('button')].find(
    (element) => element.textContent?.trim() === label
  );
  if (!result) expect.fail(`Missing ${label} button`);
  return result;
}
