import { describe, expect, it } from 'vitest';
import type { CodeAttempt } from '$lib/api/code-mode';
import { ApiProblem } from '$lib/api/http';
import {
  adapterLabel,
  clientLabel,
  mutationError,
  nativeModels,
  recoveryReasonLabel,
  refusalAdvice,
  reservationLabel,
  servingHealthLabel,
  tokenCount,
  tokenLimit,
  usageCertaintyLabel
} from './presentation';

const attempt: CodeAttempt = {
  id: '10000000-0000-4000-8000-000000000001',
  project_id: '10000000-0000-4000-8000-000000000002',
  route_id: '10000000-0000-4000-8000-000000000003',
  route_revision_id: '10000000-0000-4000-8000-000000000004',
  api_key_id: '10000000-0000-4000-8000-000000000005',
  binding_id: null,
  account_id: null,
  operation: 'responses',
  model: 'fixture-model',
  state: 'settled',
  reserved_tokens: 0,
  reported_tokens: null,
  input_tokens: null,
  output_tokens: null,
  cached_tokens: null,
  reasoning_tokens: null,
  bound_evidence: null,
  refusal: null,
  created_at: '2026-01-01T00:00:00Z',
  finished_at: '2026-01-01T00:00:01Z',
  outcome_origin: null,
  outcome: null,
  upstream_status: null,
  outcome_observed_at: null,
  end_user_digest: '',
  initial_recovery: false,
  nonexecution: false,
  recovery_from: null,
  recovery_reason: null
};

describe('code configuration input', () => {
  it('preserves native identifier spelling and order across separators', () => {
    expect(nativeModels('  Team-Model,namespace/model:v1\nsmall_2  ')).toEqual([
      'Team-Model',
      'namespace/model:v1',
      'small_2'
    ]);
  });

  it.each(['', ', \n ', 'same,same', '_invalid', 'invalid*', 'm'.repeat(201)])(
    'rejects invalid model declarations %s',
    (value) => expect(() => nativeModels(value)).toThrow()
  );

  it('accepts the declared model capacity and refuses overflow', () => {
    const models = Array.from(
      { length: 100 },
      (_, index) => `fixture-${index}`
    );
    expect(nativeModels(models.join(','))).toEqual(models);
    expect(() => nativeModels([...models, 'one-more'].join(','))).toThrow();
  });

  it('distinguishes no token cap from positive safe integer caps', () => {
    expect(tokenLimit(' \n ')).toBeNull();
    expect(tokenLimit('0012')).toBe(12);
    expect(tokenLimit(String(Number.MAX_SAFE_INTEGER))).toBe(
      Number.MAX_SAFE_INTEGER
    );
  });

  it.each([
    '0',
    '-1',
    '1.5',
    '1e3',
    '+1',
    'Infinity',
    String(Number.MAX_SAFE_INTEGER + 1)
  ])('rejects an invalid token cap %s', (value) =>
    expect(() => tokenLimit(value)).toThrow()
  );
});

describe('code diagnostics', () => {
  it('keeps missing token observations distinct from reported zero', () => {
    expect(tokenCount(null)).toBe('Unknown');
    expect(tokenCount(undefined)).toBe('Unknown');
    expect(tokenCount(0)).toBe('0');
    expect(tokenCount(12345)).toBe('12,345');
  });

  it('does not infer confirmed zero from a missing or merely reported total', () => {
    expect(usageCertaintyLabel(attempt)).toBe('Unknown consumption');
    expect(usageCertaintyLabel({ ...attempt, reported_tokens: 0 })).toBe(
      'Reported total'
    );
    expect(
      usageCertaintyLabel({
        ...attempt,
        reported_tokens: 0,
        nonexecution: true
      })
    ).toBe('Confirmed zero consumption');
    expect(usageCertaintyLabel({ ...attempt, reported_tokens: 12 })).toBe(
      'Reported total'
    );
  });

  it('distinguishes pending, undispatched and retained uncertain attempts', () => {
    const pending: CodeAttempt = { ...attempt, state: 'prepared' };
    const aborted: CodeAttempt = { ...attempt, state: 'aborted' };
    const uncertain: CodeAttempt = { ...attempt, state: 'uncertain' };
    expect(usageCertaintyLabel(pending)).toBe('Awaiting dispatch');
    expect(usageCertaintyLabel(aborted)).toBe('Not dispatched');
    expect(usageCertaintyLabel(uncertain)).toBe('Unknown consumption');
    expect(reservationLabel(pending)).toBe('Pending reservation');
    expect(reservationLabel(uncertain)).toBe('Retained uncertain reservation');
    expect(reservationLabel(attempt)).toBe('Original reservation');
    expect(
      reservationLabel({ ...attempt, state: 'bound_violation' })
    ).toContain('budgeted admission blocked');
  });

  it('reports only recorded recovery evidence', () => {
    expect(recoveryReasonLabel(attempt)).toBe('Recovery reason not recorded');
    expect(
      recoveryReasonLabel({
        ...attempt,
        recovery_reason: 'credential_rejected'
      })
    ).toBe('Credential rejection, proven not executed');
    expect(
      recoveryReasonLabel({ ...attempt, recovery_reason: 'quota_rejected' })
    ).toBe('Quota rejection, proven not executed');
  });

  it('distinguishes current serving health while retaining unfamiliar identifiers', () => {
    expect(servingHealthLabel('healthy')).toBe('Healthy');
    expect(servingHealthLabel('unknown')).toBe('Unknown');
    expect(servingHealthLabel('unavailable')).toBe(
      'Temporary upstream failure'
    );
    expect(servingHealthLabel('quota_limited')).toBe(
      'Provider rate or quota limit'
    );
    expect(adapterLabel(null)).toBe('No adapter');
    expect(adapterLabel('fixture-adapter')).toBe('fixture-adapter');
    expect(clientLabel('fixture-client')).toBe('fixture-client');
  });

  it('directs different refusal classes to their own recovery action', () => {
    expect(refusalAdvice('code_ip_not_allowed')).toContain(
      'allowed client networks'
    );
    expect(refusalAdvice('code_missing_attribution')).toContain(
      'attribution labels'
    );
    expect(refusalAdvice('code_authority_revoked')).toContain(
      'issuer and route permissions'
    );
    expect(refusalAdvice('code_end_user_required')).toContain(
      'end-user source'
    );
    expect(refusalAdvice('code_tree_retired')).toContain('permanently retired');
    expect(refusalAdvice('code_parent_unresolved')).toContain(
      'parent identifiers'
    );
    expect(refusalAdvice('code_token_bound_unavailable')).toContain(
      'proven safe token bound'
    );
    expect(refusalAdvice('code_budget_exceeded')).toContain(
      'Missing usage is not zero'
    );
    expect(refusalAdvice('code_account_unavailable')).toContain(
      'pinned account'
    );
    expect(refusalAdvice('code_model_unavailable')).toContain(
      'native model permissions'
    );
    expect(refusalAdvice('fixture_unknown')).toContain(
      'never overrides revoked access'
    );
  });

  it('preserves field feedback and identifies edits protected by a stale ETag', () => {
    const fields = new ApiProblem({
      title: 'Invalid input',
      status: 422,
      errors: { name: [{ code: 'invalid', message: 'Pick an unused name' }] }
    });
    expect(mutationError(fields)).toBe('name: Pick an unused name');
    expect(mutationError(new Error('Fixture failure'))).toBe('Fixture failure');
    const stale = new ApiProblem({
      type: 'https://openllmproxy.dev/problems/etag_mismatch',
      title: 'Resource changed',
      status: 412
    });
    expect(mutationError(stale)).toContain('Your edits have been kept');
    expect(mutationError(stale)).toContain('latest version');
  });
});
