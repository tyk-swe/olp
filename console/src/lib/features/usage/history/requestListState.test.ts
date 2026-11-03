import { describe, expect, it } from 'vitest';
import {
  requestFilters,
  requestList,
  requestProblem,
  requestSearch,
  requestState,
  requestUrl,
  type RequestForm
} from './requestListState';

const id = '123e4567-e89b-12d3-a456-426614174000';
const blank = requestList.empty();

describe('request history filters', () => {
  it('starts on the first page with no applied filters', () => {
    expect(blank.cursor).toBeUndefined();
    expect(blank.history).toEqual([]);
    expect(requestSearch(blank.applied)).toBe('');
  });

  it('round trips a shared link with filters and precise timestamps', () => {
    const source = new URLSearchParams({
      route: ' route-a ',
      provider_id: id,
      model: ' model-a ',
      api_key_id: id,
      project_id: id,
      session_id: 'session-a',
      attribution_key: 'session',
      attribution_value: 'session-a',
      operation: 'embeddings',
      status_code: '0',
      error_class: 'timeout',
      started_after: '2025-04-01T08:00:05.123456789-04:00',
      started_before: '2025-04-02T08:00:07.987654321-04:00'
    });
    const state = requestState(source);

    expect(state.applied).toMatchObject({
      route: 'route-a',
      provider_id: id,
      model: 'model-a',
      api_key_id: id,
      project_id: id,
      session_id: 'session-a',
      attribution_key: 'session',
      attribution_value: 'session-a',
      operation: 'embeddings',
      status_code: 0,
      error_class: 'timeout',
      started_after: '2025-04-01T08:00:05.123456789-04:00',
      started_before: '2025-04-02T08:00:07.987654321-04:00'
    });
    expect(requestFilters(state, state.applied)).toEqual(state.applied);
    expect(requestProblem(state, false, state.applied)).toBeNull();
    expect(
      requestState(new URLSearchParams(requestSearch(state.applied))).applied
    ).toEqual(state.applied);
    expect(requestUrl.canonical(source)).toBe(requestSearch(state.applied));
  });

  it('retains invalid shared-link input for correction', () => {
    const source = new URLSearchParams({ started_after: 'not-a-time' });
    const state = requestState(source);

    expect(state.startedAfter).toBe('not-a-time');
    expect(requestProblem(state, true)).toContain('Started after');
    expect(requestUrl.canonical(source)).toBeNull();
  });

  it.each([
    [{ providerId: 'bad' }, 'Provider ID must be a UUID: bad'],
    [{ apiKeyId: 'bad' }, 'API key ID must be a UUID: bad'],
    [{ projectId: 'bad' }, 'Project ID must be a UUID: bad'],
    [
      { sessionId: 'one', attributionKey: 'team' },
      'The session filter conflicts with the attribution key.'
    ],
    [
      { sessionId: 'one', attributionValue: 'two' },
      'The session filter conflicts with the attribution value.'
    ],
    [{ operation: 'unknown' }, 'Unknown operation: unknown'],
    [
      { statusCode: '-1' },
      'Status code must be an integer from 0 to 65535: -1'
    ],
    [
      { statusCode: '1.5' },
      'Status code must be an integer from 0 to 65535: 1.5'
    ],
    [
      { statusCode: '65536' },
      'Status code must be an integer from 0 to 65535: 65536'
    ],
    [
      { startedAfter: '2025-04-02T00:00', startedBefore: '2025-04-01T00:00' },
      'Started before must be later than started after.'
    ]
  ] satisfies [Partial<RequestForm>, string][])(
    'rejects invalid filters %o',
    (fields, error) => {
      expect(requestProblem({ ...blank, ...fields })).toBe(error);
    }
  );

  it('accepts the upper status-code boundary', () => {
    expect(requestProblem({ ...blank, statusCode: '65535' })).toBeNull();
    expect(requestFilters({ ...blank, statusCode: '65535' }).status_code).toBe(
      65535
    );
  });
});
