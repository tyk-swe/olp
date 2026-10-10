import { afterEach, describe, expect, it, vi } from 'vitest';
import { authLifecycle } from '$lib/features/access/session/lifecycle';
import { clearCsrfToken } from '$lib/features/access/session/csrf';
import { captureRequests, jsonResponse } from '$lib/api/test/requestCapture';
import { exportRequestsCsv } from '$lib/features/usage/history/api';
import { exportUsageCsv } from '$lib/features/usage/api/usage';

const session = {
  user: {
    id: '01980000-0000-7000-8000-000000000401',
    email: 'operator@example.com',
    display_name: 'Operator',
    role: 'operator' as const,
    access_scope: 'global' as const,
    operations: []
  },
  csrf_token: 'csrf-csv-token'
};

afterEach(async () => {
  await authLifecycle.principalInvalidated();
  clearCsrfToken();
  vi.unstubAllGlobals();
});

describe('CSV exports', () => {
  it('downloads the requests CSV with the active filters as text', async () => {
    authLifecycle.establishSession(session);
    const requests = captureRequests(
      () =>
        new Response('request_id,route\r\nabc,primary\r\n', {
          status: 200,
          headers: { 'content-type': 'text/csv' }
        })
    );

    const csv = await exportRequestsCsv({
      route: 'primary',
      project_id: '01980000-0000-7000-8000-000000000901',
      session_id: 'sess-42'
    });

    expect(csv).toContain('request_id');
    const url = new URL(requests[0]!.url);
    expect(url.pathname).toBe('/api/v1/requests/export.csv');
    expect(url.searchParams.get('route')).toBe('primary');
    expect(url.searchParams.get('project_id')).toBe(
      '01980000-0000-7000-8000-000000000901'
    );
    expect(url.searchParams.get('session_id')).toBe('sess-42');
  });

  it('downloads the usage CSV with the current dimension', async () => {
    authLifecycle.establishSession(session);
    const requests = captureRequests(
      () =>
        new Response('row_type,request_count\r\nsummary,1\r\n', {
          status: 200,
          headers: { 'content-type': 'text/csv' }
        })
    );

    const csv = await exportUsageCsv(
      {
        start: '2026-10-08T00:00:00Z',
        end: '2026-10-09T00:00:00Z',
        session_id: 'sess-42'
      },
      'session'
    );

    expect(csv).toContain('summary');
    const url = new URL(requests[0]!.url);
    expect(url.pathname).toBe('/api/v1/usage/export.csv');
    expect(url.searchParams.get('dimension')).toBe('session');
    expect(url.searchParams.get('session_id')).toBe('sess-42');
  });

  it('surfaces a 422 problem when the export is too large', async () => {
    authLifecycle.establishSession(session);
    captureRequests(() =>
      jsonResponse(
        {
          type: 'urn:olp:problem:export_too_large',
          status: 422,
          detail: 'Narrow the filters'
        },
        { status: 422 }
      )
    );

    await expect(exportRequestsCsv({})).rejects.toMatchObject({
      problem: expect.objectContaining({ status: 422 })
    });
  });
});
