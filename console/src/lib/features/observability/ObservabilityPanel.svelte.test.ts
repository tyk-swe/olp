// @vitest-environment jsdom
import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { operationsFor } from '$lib/features/access/session/test/grants';
import {
  createCapturePolicy,
  deleteExportSink,
  getCaptureConfiguration,
  listCapturePolicies,
  listCaptureSinkOptions,
  listExportSinks,
  listExportSinkGaps,
  updateCapturePolicy,
  updateExportSink
} from '$lib/features/observability/api';
import type {
  CapturePolicy,
  ExportSink
} from '$lib/features/observability/api';
import ObservabilityProbe from './test/ObservabilityProbe.svelte';

const role = vi.hoisted(() => ({
  current: 'owner' as 'owner' | 'operator' | 'developer' | 'viewer',
  global: true,
  ops: null as string[] | null
}));

vi.mock('$lib/features/access/session/useRole.svelte', async () => {
  const { can, allows } =
    await import('$lib/features/access/session/authorization');
  return {
    useRole: () => {
      const grant = {
        operations: (role.ops ??
          operationsFor(role.current, role.global)) as never[],
        access_scope: (role.global ? 'global' : 'assigned') as
          'global' | 'assigned'
      };
      return {
        role: role.current,
        globalScope: role.global,
        user: {
          role: role.current,
          access_scope: role.global ? 'global' : 'assigned',
          operations: grant.operations
        },
        can: (capability: never) => can(grant, capability),
        allows: (route: never) => allows(grant, route)
      };
    }
  };
});
vi.mock('$lib/features/access/session/serviceCapabilities.svelte', () => ({
  useServiceCapabilities: () => ({ payloadCaptureActive: true })
}));
vi.mock('$lib/features/access/projects/api', async (original) => ({
  ...(await original<typeof import('$lib/features/access/projects/api')>()),
  listProjectMemberships: vi.fn(async () => [
    {
      id: '01980000-0000-7000-8000-000000000901',
      name: 'Platform',
      role: 'manager' as const
    }
  ])
}));
vi.mock('$lib/features/observability/api', () => ({
  createCapturePolicy: vi.fn(),
  createExportSink: vi.fn(),
  deleteCapturePolicy: vi.fn(),
  deleteExportSink: vi.fn(),
  getCaptureConfiguration: vi.fn(),
  listCapturePolicies: vi.fn(),
  listCaptureSinkOptions: vi.fn(),
  listExportSinks: vi.fn(),
  listExportSinkGaps: vi.fn(),
  updateCaptureConfiguration: vi.fn(),
  updateCapturePolicy: vi.fn(),
  updateExportSink: vi.fn()
}));

const projectId = '01980000-0000-7000-8000-000000000901';
const sinkId = '01980000-0000-7000-8000-000000000601';

const httpsSink: ExportSink = {
  export_sink_id: sinkId,
  name: 'Collector',
  type: 'https',
  destination: 'https://collector.example.com/olp',
  project_id: null,
  streams: ['requests', 'usage_rollups'],
  format: 'json',
  filter: null,
  enabled: true,
  credential_configured: true,
  etag: 'sink-etag-1',
  status: [
    {
      stream: 'requests',
      pending: 4,
      pending_error_codes: ['timeout'],
      delivered_total: 10,
      failures_total: 2,
      gaps_total: 1,
      last_success_at: '2026-10-08T12:00:00Z',
      last_attempt_at: '2026-10-08T12:30:00Z',
      oldest_pending_at: '2026-10-08T12:29:00Z'
    },
    {
      stream: 'usage_rollups',
      pending: 0,
      pending_error_codes: [],
      delivered_total: 3,
      failures_total: 0,
      gaps_total: 0,
      last_success_at: '2026-10-08T12:15:00Z',
      last_attempt_at: null,
      oldest_pending_at: null
    }
  ]
};

const projectSink: ExportSink = {
  ...httpsSink,
  export_sink_id: '01980000-0000-7000-8000-000000000602',
  name: 'Project sink',
  project_id: projectId
};

const foreignSink: ExportSink = {
  ...httpsSink,
  export_sink_id: '01980000-0000-7000-8000-000000000603',
  name: 'Foreign sink',
  project_id: '01980000-0000-7000-8000-000000000999'
};

const projectPolicy: CapturePolicy = {
  id: '01980000-0000-7000-8000-000000000701',
  project_id: projectId,
  route_slug: 'primary',
  sink: sinkId,
  sample_ratio: '0.5',
  include: ['input', 'output'],
  redact: [],
  key_ids: [],
  end_user_digests: [],
  max_bytes: 65536,
  enabled: true,
  etag: 'policy-etag-1'
};

let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount> | undefined;

beforeEach(() => {
  vi.clearAllMocks();
  role.current = 'owner';
  role.global = true;
  host = document.createElement('div');
  document.body.append(host);
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } }
  });
  vi.mocked(listExportSinks).mockResolvedValue([httpsSink]);
  vi.mocked(listCapturePolicies).mockResolvedValue([projectPolicy]);
  vi.mocked(listCaptureSinkOptions).mockResolvedValue([
    { export_sink_id: sinkId, name: 'Collector', type: 'https' }
  ]);
  vi.mocked(getCaptureConfiguration).mockResolvedValue({
    enabled: true,
    etag: 'cap-etag-1'
  });
  vi.mocked(listExportSinkGaps).mockResolvedValue([
    {
      id: '01980000-0000-7000-8000-000000000801',
      stream: 'requests',
      record_count: 3,
      first_occurred_at: '2026-10-08T10:00:00Z',
      last_occurred_at: '2026-10-08T11:00:00Z',
      reason: 'retention_expired'
    }
  ]);
});

afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  client.clear();
  host.remove();
});

async function render() {
  component = mount(ObservabilityProbe, { target: host, props: { client } });
  flushSync();
  await new Promise((resolve) => setTimeout(resolve, 0));
  await new Promise((resolve) => setTimeout(resolve, 0));
  flushSync();
}

function choose(selector: string, value: string) {
  const field = host.querySelector<
    HTMLInputElement | HTMLSelectElement | HTMLTextAreaElement
  >(selector)!;
  field.value = value;
  field.dispatchEvent(
    new Event(field instanceof HTMLSelectElement ? 'change' : 'input', {
      bubbles: true
    })
  );
  flushSync();
}

function clickButton(text: string, index = 0) {
  const matches = [
    ...host.querySelectorAll<HTMLButtonElement>('button')
  ].filter((b) => b.textContent === text);
  matches[index]!.click();
  flushSync();
}

it('renders every stream status with pending, errors, lag and gaps', async () => {
  await render();

  const text = host.textContent!;
  expect(text).toContain('requests');
  expect(text).toContain('usage_rollups');
  expect(text).toContain('4 pending');
  expect(text).toContain('errors: timeout');
  expect(text).toContain('10 delivered');
  expect(text).toContain('approx. lag');
});

it('owner edits a sink PATCHing mutable fields under its ETag', async () => {
  vi.mocked(updateExportSink).mockResolvedValue(httpsSink);
  await render();

  clickButton('Edit');
  choose('#sink-name', 'Collector v2');
  choose('#sink-destination', 'https://collector2.example.com');
  choose('#sink-filter-route', 'primary');
  choose('#sink-filter-outcome', 'success');
  choose('#sink-filter-project', projectId);
  choose('#sink-credential', '{"token":"new"}');
  host
    .querySelector<HTMLFormElement>('form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await new Promise((resolve) => setTimeout(resolve, 0));

  expect(updateExportSink).toHaveBeenCalledWith(httpsSink, {
    name: 'Collector v2',
    destination: 'https://collector2.example.com',
    filter: { project: projectId, route: 'primary', outcome: 'success' },
    credential: { token: 'new' }
  });
});

it('shows gap records for every reader', async () => {
  Object.assign(role, { current: 'viewer' as const });
  await render();

  clickButton('Gaps');
  await vi.waitFor(() =>
    expect(host.textContent).toContain('retention_expired')
  );
  expect(listExportSinkGaps).toHaveBeenCalledWith(
    sinkId,
    expect.any(AbortSignal)
  );
});

it('project manager edits only matching project resources', async () => {
  Object.assign(role, { current: 'developer' as const, global: false });
  vi.mocked(listExportSinks).mockResolvedValue([
    projectSink,
    foreignSink,
    httpsSink
  ]);
  await render();

  const rows = [...host.querySelectorAll('tbody tr')];
  const projectRow = rows.find((r) => r.textContent!.includes('Project sink'))!;
  const foreignRow = rows.find((r) => r.textContent!.includes('Foreign sink'))!;
  const installRow = rows.find((r) => r.textContent!.includes('Collector'))!;
  expect(projectRow.textContent).toContain('Edit');
  expect(foreignRow.textContent).not.toContain('Edit');
  expect(installRow.textContent).not.toContain('Edit');
});

it('edits a capture policy under its ETag', async () => {
  vi.mocked(updateCapturePolicy).mockResolvedValue({
    ...projectPolicy,
    sample_ratio: '0.1'
  });
  await render();

  clickButton('Edit', 1);
  choose('#policy-ratio', '0.1');
  choose('#policy-max', '1024');
  host
    .querySelectorAll<HTMLFormElement>('form')[1]!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await new Promise((resolve) => setTimeout(resolve, 0));

  expect(updateCapturePolicy).toHaveBeenCalledWith(
    projectPolicy,
    expect.objectContaining({ sample_ratio: '0.1', max_bytes: 1024 })
  );
});

it('rejects invalid credential JSON without sending', async () => {
  await render();

  clickButton('Edit');
  choose('#sink-name', 'Collector v2');
  choose('#sink-destination', 'https://collector2.example.com');
  choose('#sink-credential', '{bad json');
  host
    .querySelector<HTMLFormElement>('form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await new Promise((resolve) => setTimeout(resolve, 0));

  expect(updateExportSink).not.toHaveBeenCalled();
  expect(host.textContent).toContain('not valid JSON');
});

it('deletes a sink under its ETag', async () => {
  vi.mocked(deleteExportSink).mockResolvedValue(undefined);
  await render();

  clickButton('Delete');
  await new Promise((resolve) => setTimeout(resolve, 0));
  expect(deleteExportSink).toHaveBeenCalledWith(httpsSink);
});

it('viewer sees rows and Gaps but no mutation controls', async () => {
  Object.assign(role, { current: 'viewer' as const });
  await render();

  expect(host.querySelector('form')).toBeNull();
  const tableText = host.querySelector('table')!.textContent!;
  expect(tableText).toContain('Collector');
  expect(
    [...host.querySelectorAll<HTMLButtonElement>('button')].filter((b) =>
      ['Edit', 'Disable', 'Enable', 'Delete'].includes(b.textContent!)
    )
  ).toHaveLength(0);
  // Gaps remains for readers
  clickButton('Gaps');
  await vi.waitFor(() =>
    expect(host.textContent).toContain('retention_expired')
  );
});

it('disables a policy with a pure enabled:false PATCH while master is off', async () => {
  vi.mocked(getCaptureConfiguration).mockResolvedValue({
    enabled: false,
    etag: 'cap-etag-off'
  });
  vi.mocked(updateCapturePolicy).mockResolvedValue({
    ...projectPolicy,
    enabled: false
  });
  await render();

  clickButton('Disable', 1);
  await new Promise((resolve) => setTimeout(resolve, 0));
  expect(updateCapturePolicy).toHaveBeenCalledWith(projectPolicy, {
    enabled: false
  });
});

it('clears stored selectors explicitly with empty arrays', async () => {
  const withSelectors: CapturePolicy = {
    ...projectPolicy,
    key_ids: ['01980000-0000-7000-8000-000000000903'],
    end_user_digests: ['digest-1']
  };
  vi.mocked(listCapturePolicies).mockResolvedValue([withSelectors]);
  vi.mocked(updateCapturePolicy).mockResolvedValue(withSelectors);
  await render();

  clickButton('Edit', 1);
  choose('#policy-keys', '');
  choose('#policy-users', '');
  host
    .querySelectorAll<HTMLFormElement>('form')[1]!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await new Promise((resolve) => setTimeout(resolve, 0));

  expect(updateCapturePolicy).toHaveBeenCalledWith(
    withSelectors,
    expect.objectContaining({ key_ids: [], end_user_digests: [] })
  );
});

it('blocks an assigned manager from creating an installation policy', async () => {
  Object.assign(role, { current: 'developer' as const, global: false });
  await render();
  flushSync();

  const select = host.querySelector<HTMLSelectElement>('#policy-project');
  expect(select).not.toBeNull();
  const options = [...select!.querySelectorAll('option')].map(
    (o) => o.textContent
  );
  expect(options).not.toContain('Installation-wide');

  const form = [...host.querySelectorAll<HTMLFormElement>('form')].find(
    (candidate) => candidate.querySelector('#policy-project')
  )!;
  form.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await new Promise((resolve) => setTimeout(resolve, 0));
  expect(vi.mocked(createCapturePolicy)).not.toHaveBeenCalled();
});

it('lets a global settings holder create an installation policy', async () => {
  role.ops = ['settings'];
  await render();
  flushSync();

  const form = [...host.querySelectorAll<HTMLFormElement>('form')].find(
    (candidate) => candidate.querySelector('#policy-project')
  )!;
  form.querySelector<HTMLSelectElement>('#policy-sink')!.value = sinkId;
  form
    .querySelector<HTMLSelectElement>('#policy-sink')!
    .dispatchEvent(new Event('change', { bubbles: true }));
  form.querySelector<HTMLInputElement>('#policy-ratio')!.value = '0.5';
  form
    .querySelector<HTMLInputElement>('#policy-ratio')!
    .dispatchEvent(new Event('input', { bubbles: true }));
  form.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await new Promise((resolve) => setTimeout(resolve, 0));
  expect(vi.mocked(createCapturePolicy)).toHaveBeenCalled();
});

it('keeps credential blank omitted and sends explicit null only on clear', async () => {
  vi.mocked(updateExportSink).mockResolvedValue(httpsSink);
  await render();

  clickButton('Edit');
  choose('#sink-name', 'Renamed');
  host
    .querySelector<HTMLFormElement>('form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await new Promise((resolve) => setTimeout(resolve, 0));
  expect(updateExportSink).toHaveBeenLastCalledWith(
    httpsSink,
    expect.not.objectContaining({ credential: expect.anything() })
  );
  expect(vi.mocked(updateExportSink).mock.calls.at(-1)![1]).not.toHaveProperty(
    'credential'
  );

  clickButton('Edit');
  const clear = host.querySelector<HTMLInputElement>(
    'input[aria-label="Clear stored credential"]'
  )!;
  clear.checked = true;
  clear.dispatchEvent(new Event('change', { bubbles: true }));
  host
    .querySelector<HTMLFormElement>('form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await new Promise((resolve) => setTimeout(resolve, 0));
  expect(updateExportSink).toHaveBeenLastCalledWith(
    httpsSink,
    expect.objectContaining({ credential: null })
  );
});

it('a slow gap response cannot populate another sink', async () => {
  const other: ExportSink = {
    ...httpsSink,
    export_sink_id: '01980000-0000-7000-8000-000000000604',
    name: 'Second sink'
  };
  vi.mocked(listExportSinks).mockResolvedValue([httpsSink, other]);
  let resolveA!: (value: never[]) => void;
  vi.mocked(listExportSinkGaps)
    .mockImplementationOnce(() => new Promise((r) => (resolveA = r as never)))
    .mockResolvedValueOnce([
      {
        id: '01980000-0000-7000-8000-000000000802',
        stream: 'requests',
        record_count: 1,
        first_occurred_at: '2026-10-08T10:00:00Z',
        last_occurred_at: '2026-10-08T11:00:00Z',
        reason: 'other-gap'
      }
    ]);
  await render();

  clickButton('Gaps', 0);
  flushSync();
  clickButton('Gaps', 1);
  await vi.waitFor(() => expect(host.textContent).toContain('other-gap'));
  resolveA([]);
  await new Promise((resolve) => setTimeout(resolve, 0));
  expect(host.textContent).toContain('other-gap');
  expect(host.textContent).not.toContain('retention_expired');
});

it('surfaces an ETag conflict on save', async () => {
  const { ApiProblem } = await import('$lib/api/http');
  vi.mocked(updateExportSink).mockRejectedValue(
    new ApiProblem({
      type: 'urn:olp:problem:precondition_failed',
      title: 'The resource changed underneath you',
      status: 412
    })
  );
  await render();

  clickButton('Edit');
  choose('#sink-name', 'Renamed');
  host
    .querySelector<HTMLFormElement>('form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await vi.waitFor(() =>
    expect(host.querySelector('[role="alert"]')).not.toBeNull()
  );
});
