// @vitest-environment jsdom
import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { operationsFor } from '$lib/features/access/session/test/grants';
import {
  createExportSink,
  createCapturePolicy,
  deleteExportSink,
  getCaptureConfiguration,
  listCapturePolicies,
  listCaptureRedactorOptions,
  listCaptureSinkOptions,
  listExportSinks,
  listExportSinkFocusDatasets,
  listExportSinkGaps,
  updateCapturePolicy,
  updateExportSink
} from '$lib/features/observability/api';
import type {
  CapturePolicy,
  ExportSink
} from '$lib/features/observability/api';
import ObservabilityProbe from './test/ObservabilityProbe.svelte';
import type { MutationIntent } from '$lib/forms/mutationIntent';

const receipts = vi.hoisted(() => [] as MutationIntent[]);
vi.mock('$lib/forms/mutationIntent.svelte', async (original) => {
  const helpers =
    await original<typeof import('$lib/forms/mutationIntent.svelte')>();
  return {
    ...helpers,
    useMutationIntent() {
      const receipt = helpers.useMutationIntent();
      receipts.push(receipt);
      return receipt;
    }
  };
});

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
  listCaptureRedactorOptions: vi.fn().mockResolvedValue([]),
  listCaptureSinkOptions: vi.fn(),
  listExportSinks: vi.fn(),
  listExportSinkFocusDatasets: vi.fn(),
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

const focusSink: ExportSink = {
  ...httpsSink,
  export_sink_id: '01980000-0000-7000-8000-000000000610',
  name: 'FOCUS export',
  type: 's3',
  streams: ['focus'],
  format: 'csv_gzip',
  settings: { cost_basis: 'cost', start_date: '2026-10-01' },
  status: [
    {
      stream: 'focus',
      delivered_days: 2,
      latest_delivered_date: '2026-10-08',
      last_success_at: '2026-10-08T12:00:00Z',
      pending_error_codes: []
    }
  ]
};

const focusDatasets = [
  {
    charge_date: '2026-10-08',
    commitment_totals: {
      purchased_cost: '10',
      allocated_cost: '7.5',
      unused_cost: '2.5',
      provisional: true,
      complete: false
    },
    revision: 2,
    generated_at: '2026-10-09T03:00:00Z',
    delivered_at: '2026-10-09T03:05:00Z',
    row_count: 128,
    part_count: 1,
    billed_cost: '12.5',
    effective_cost: '10',
    attempts: 1,
    last_error_code: null,
    current: true
  },
  {
    charge_date: '2026-10-07',
    commitment_totals: null,
    revision: 1,
    generated_at: null,
    delivered_at: null,
    row_count: 0,
    part_count: 0,
    billed_cost: null,
    effective_cost: null,
    attempts: 2,
    last_error_code: 'status',
    current: false
  }
];

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
  vi.mocked(listCaptureRedactorOptions).mockResolvedValue([]);
  receipts.length = 0;
  role.ops = null;
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

it('retires the pending credential receipt when replacing sink creation with an existing sink draft', async () => {
  vi.mocked(createExportSink).mockRejectedValue(new TypeError('Response lost'));
  await render();
  choose('#sink-name', 'New collector');
  choose('#sink-destination', 'https://collector.example/new');
  choose('#sink-credential', '{"token":"ephemeral"}');
  const form = host.querySelector('form')!;
  form.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await vi.waitFor(() => expect(host.textContent).toContain('Response lost'));
  expect(receipts[0].unresolved).toBe(true);
  clickButton('Edit');
  expect(
    host.querySelector<HTMLTextAreaElement>('#sink-credential')!.value
  ).toBe('');
  expect(receipts[0].unresolved).toBe(false);
});

it.each(['sink', 'policy'])(
  'keeps the submitted %s draft frozen until delayed write completion',
  async (kind) => {
    let finish!: () => void;
    if (kind === 'sink')
      vi.mocked(updateExportSink).mockImplementation(
        () =>
          new Promise((resolve) => {
            finish = () => resolve(httpsSink);
          })
      );
    else
      vi.mocked(updateCapturePolicy).mockImplementation(
        () =>
          new Promise((resolve) => {
            finish = () => resolve(projectPolicy);
          })
      );
    await render();
    clickButton('Edit', kind === 'sink' ? 0 : 1);
    const selector = kind === 'sink' ? '#sink-name' : '#policy-ratio';
    const value = kind === 'sink' ? 'Submitted collector' : '0.125';
    choose(selector, value);
    host
      .querySelectorAll('form')
      .item(kind === 'sink' ? 0 : 1)
      .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    flushSync();
    const activeField = host.querySelector<HTMLInputElement>(selector)!;
    expect(activeField.matches(':disabled')).toBe(true);
    const row = [...host.querySelectorAll('tbody tr')].find((candidate) =>
      candidate.textContent?.includes(kind === 'sink' ? 'Collector' : '0.5')
    )!;
    const edit = [...row.querySelectorAll<HTMLButtonElement>('button')].find(
      (b) => b.textContent === 'Edit'
    )!;
    expect(edit.disabled).toBe(true);
    edit.click();
    clickButton('Cancel');
    expect(activeField.value).toBe(value);
    finish();
    await vi.waitFor(() =>
      expect(host.textContent).toContain(
        kind === 'sink' ? 'Sink updated.' : 'Capture policy updated.'
      )
    );
  }
);

it('keeps a sink create retry stable but reports refresh failure after a known write', async () => {
  vi.mocked(createExportSink)
    .mockRejectedValueOnce(new TypeError('Response lost'))
    .mockResolvedValueOnce(httpsSink);
  await render();
  choose('#sink-name', 'New collector');
  choose('#sink-destination', 'https://collector.example/new');
  const form = host.querySelector('form')!;
  form.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await vi.waitFor(() => expect(host.textContent).toContain('Response lost'));
  vi.spyOn(client, 'invalidateQueries').mockRejectedValueOnce(
    new Error('Refresh unavailable')
  );
  form.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await vi.waitFor(() =>
    expect(host.textContent).toContain('Refresh unavailable')
  );
  expect(vi.mocked(createExportSink).mock.calls[1]).toEqual(
    vi.mocked(createExportSink).mock.calls[0]
  );
  expect(host.textContent).toContain('Sink created.');
  expect(host.querySelector<HTMLInputElement>('#sink-name')!.value).toBe('');
  expect(receipts[0].unresolved).toBe(false);
});

it.each(['sink', 'policy'])(
  'rejects delayed %s write callbacks after the form owner is destroyed',
  async (kind) => {
    let finish!: () => void;
    if (kind === 'sink')
      vi.mocked(createExportSink).mockImplementation(
        () =>
          new Promise((resolve) => {
            finish = () => resolve(httpsSink);
          })
      );
    else
      vi.mocked(createCapturePolicy).mockImplementation(
        () =>
          new Promise((resolve) => {
            finish = () => resolve(projectPolicy);
          })
      );
    await render();
    if (kind === 'sink') {
      choose('#sink-name', 'Deferred collector');
      choose('#sink-destination', 'https://collector.example/deferred');
    } else choose('#policy-sink', sinkId);
    host
      .querySelectorAll('form')
      .item(kind === 'sink' ? 0 : 1)
      .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    flushSync();
    const invalidate = vi.spyOn(client, 'invalidateQueries');
    await unmount(component!);
    component = undefined;
    finish();
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(invalidate).not.toHaveBeenCalled();
    expect(receipts.every((receipt) => !receipt.unresolved)).toBe(true);
  }
);

it('locks streams for cloudevents, stripe and csv_gzip formats', async () => {
  await render();

  choose('#sink-format', 'json');
  choose('#sink-type', 's3');
  choose('#sink-format', 'csv_gzip');
  const focusCheckbox = [
    ...host.querySelectorAll<HTMLInputElement>('input[type="checkbox"]')
  ].find((box) => box.closest('label')?.textContent === 'FOCUS')!;
  expect(focusCheckbox.checked).toBe(true);
  expect(focusCheckbox.disabled).toBe(true);
  // The outcome filter is hidden for FOCUS.
  expect(host.querySelector('#sink-filter-outcome')).toBeNull();

  choose('#sink-format', 'jsonl');
  const focusGone = [
    ...host.querySelectorAll<HTMLInputElement>('input[type="checkbox"]')
  ].find((box) => box.closest('label')?.textContent === 'FOCUS');
  expect(focusGone).toBeUndefined();

  choose('#sink-type', 'stripe_meter');
  const metering = [
    ...host.querySelectorAll<HTMLInputElement>('input[type="checkbox"]')
  ].find((box) => box.closest('label')?.textContent === 'Metering')!;
  expect(metering.checked).toBe(true);
  expect(metering.disabled).toBe(true);
  expect(host.querySelector<HTMLInputElement>('#sink-destination')!.value).toBe(
    'https://api.stripe.com'
  );
});

it('creates a FOCUS sink with cost basis and start date settings', async () => {
  vi.mocked(createExportSink).mockResolvedValue(focusSink);
  await render();

  choose('#sink-name', 'FOCUS');
  choose('#sink-type', 's3');
  choose('#sink-format', 'csv_gzip');
  choose('#sink-destination', 'https://objects.example.com/bucket');
  choose('#sink-cost-basis', 'charge');
  choose('#sink-focus-start', '2026-10-01');
  host
    .querySelector<HTMLFormElement>('form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await new Promise((resolve) => setTimeout(resolve, 0));

  expect(createExportSink).toHaveBeenCalledWith(
    expect.objectContaining({
      type: 's3',
      format: 'csv_gzip',
      streams: ['focus'],
      settings: { cost_basis: 'charge', start_date: '2026-10-01' }
    }),
    expect.any(String)
  );
});

it('creates a CloudEvents sink locked to metering with a source', async () => {
  vi.mocked(createExportSink).mockResolvedValue(httpsSink);
  await render();

  choose('#sink-name', 'Events');
  choose('#sink-format', 'cloudevents');
  choose('#sink-destination', 'https://events.example.com');
  choose('#sink-ce-source', 'https://example.com/olp');
  host
    .querySelector<HTMLFormElement>('form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await new Promise((resolve) => setTimeout(resolve, 0));

  expect(createExportSink).toHaveBeenCalledWith(
    expect.objectContaining({
      format: 'cloudevents',
      streams: ['metering'],
      settings: { source: 'https://example.com/olp' }
    }),
    expect.any(String)
  );
});

it('sends blank CloudEvents settings as null on create and omits them on edit', async () => {
  const sink: ExportSink = {
    ...httpsSink,
    format: 'cloudevents',
    streams: ['metering'],
    settings: { source: '/original' }
  };
  vi.mocked(listExportSinks).mockResolvedValue([sink]);
  vi.mocked(createExportSink).mockResolvedValue(sink);
  vi.mocked(updateExportSink).mockResolvedValue(sink);
  await render();

  choose('#sink-name', 'Events');
  choose('#sink-format', 'cloudevents');
  choose('#sink-destination', 'https://events.example.com');
  choose('#sink-ce-source', '  ');
  host
    .querySelector<HTMLFormElement>('form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await new Promise((resolve) => setTimeout(resolve, 0));
  expect(createExportSink).toHaveBeenCalledWith(
    expect.objectContaining({ settings: null }),
    expect.any(String)
  );

  clickButton('Edit');
  choose('#sink-ce-source', '  ');
  host
    .querySelector<HTMLFormElement>('form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await new Promise((resolve) => setTimeout(resolve, 0));
  expect(updateExportSink).toHaveBeenCalledWith(sink, {
    name: sink.name,
    destination: sink.destination,
    filter: null
  });
});

it('creates a Stripe sink with the api_key credential and customer map', async () => {
  vi.mocked(createExportSink).mockResolvedValue(httpsSink);
  await render();

  choose('#sink-name', 'Stripe');
  choose('#sink-type', 'stripe_meter');
  choose('#sink-stripe-key', 'rk_test_123');
  choose('#sink-stripe-event', 'olp.charge');
  choose('#sink-stripe-value', 'charge');
  const projectSelect = host.querySelector<HTMLSelectElement>(
    '.customer-row select'
  )!;
  projectSelect.value = projectId;
  projectSelect.dispatchEvent(new Event('change', { bubbles: true }));
  flushSync();
  const customerInput = host.querySelector<HTMLInputElement>(
    '.customer-row input'
  )!;
  customerInput.value = 'cus_ABC123';
  customerInput.dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();
  clickButton('Advanced');
  choose('#sink-stripe-customer-key', 'olp_project');
  choose('#sink-stripe-value-key', 'olp_value');
  host
    .querySelector<HTMLFormElement>('form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await new Promise((resolve) => setTimeout(resolve, 0));

  expect(createExportSink).toHaveBeenCalledWith(
    expect.objectContaining({
      type: 'stripe_meter',
      format: 'stripe',
      streams: ['metering'],
      credential: { api_key: 'rk_test_123' },
      settings: {
        event_name: 'olp.charge',
        value: 'charge',
        customers: { [projectId]: 'cus_ABC123' },
        customer_key: 'olp_project',
        value_key: 'olp_value'
      }
    }),
    expect.any(String)
  );
});

it('requires a Stripe key and at least one customer mapping', async () => {
  await render();
  choose('#sink-name', 'Stripe');
  choose('#sink-type', 'stripe_meter');
  host
    .querySelector<HTMLFormElement>('form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await new Promise((resolve) => setTimeout(resolve, 0));
  expect(createExportSink).not.toHaveBeenCalled();
  expect(host.textContent).toContain('restricted API key');

  choose('#sink-stripe-key', 'rk_test_123');
  host
    .querySelector<HTMLFormElement>('form')!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await new Promise((resolve) => setTimeout(resolve, 0));
  expect(createExportSink).not.toHaveBeenCalled();
  expect(host.textContent).toContain('Map at least one project');
});

it('shows a FOCUS sink settings summary and its datasets table', async () => {
  vi.mocked(listExportSinks).mockResolvedValue([focusSink]);
  vi.mocked(listExportSinkFocusDatasets).mockResolvedValue(focusDatasets);
  await render();

  expect(host.textContent).toContain('Cost basis: cost');
  expect(host.textContent).toContain('from 2026-10-01');

  clickButton('Datasets');
  await vi.waitFor(() => expect(host.textContent).toContain('2026-10-08'));
  const text = host.textContent!;
  expect(text).toContain('Current');
  expect(text).toContain('status');
  expect(text).toContain('128');
  expect(text).toContain('Purchased: 10');
  expect(text).toContain('Allocated: 7.5');
  expect(text).toContain('Unused: 2.5');
  expect(text).toContain('Provisional');
  expect(text).toContain('Incomplete coverage');
  expect(listExportSinkFocusDatasets).toHaveBeenCalledWith(
    focusSink.export_sink_id,
    expect.any(AbortSignal)
  );
});

it('preserves pins on unrelated edits and explicitly refreshes them on save', async () => {
  const pinned: CapturePolicy = {
    ...projectPolicy,
    redact: ['private'],
    redactor_revisions: [
      {
        name: 'private',
        guardrail_id: 'g',
        revision_id: 'r1',
        revision: 1,
        type: 'builtin.pii'
      }
    ]
  };
  vi.mocked(listCapturePolicies).mockResolvedValue([pinned]);
  vi.mocked(updateCapturePolicy).mockResolvedValue(pinned);
  await render();
  clickButton('Edit', 1);
  expect(host.textContent).toContain('revision 1');
  choose('#policy-ratio', '0.1');
  host
    .querySelectorAll<HTMLFormElement>('form')[1]!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await new Promise((resolve) => setTimeout(resolve, 0));
  expect(vi.mocked(updateCapturePolicy).mock.calls[0][1]).not.toHaveProperty(
    'redact'
  );
  clickButton('Edit', 1);
  clickButton('Refresh pinned revisions on save');
  host
    .querySelectorAll<HTMLFormElement>('form')[1]!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await new Promise((resolve) => setTimeout(resolve, 0));
  expect(vi.mocked(updateCapturePolicy).mock.calls[1][1]).toHaveProperty(
    'redact',
    ['private']
  );
});

it('saves the selected detector order', async () => {
  vi.mocked(updateCapturePolicy).mockResolvedValue(projectPolicy);
  await render();
  clickButton('Edit', 1);
  choose('#policy-redactor', 'private');
  clickButton('Add redactor');
  choose('#policy-redactor', 'secrets');
  clickButton('Add redactor');
  host
    .querySelector<HTMLButtonElement>('[aria-label="Move secrets up"]')!
    .click();
  flushSync();
  host
    .querySelectorAll<HTMLFormElement>('form')[1]!
    .dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await new Promise((resolve) => setTimeout(resolve, 0));
  expect(vi.mocked(updateCapturePolicy).mock.calls[0][1]).toHaveProperty(
    'redact',
    ['secrets', 'private']
  );
});
