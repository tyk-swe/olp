import { describe, expect, it } from 'vitest';
import type { CodeAttempt } from '$lib/management/code-mode';
import { ApiProblem } from '$lib/management/http';
import {
  adapterLabel,
  allowanceChargeSummary,
  allowanceEvidenceLabel,
  allowanceWindowSummary,
  clientLabel,
  cooldownClassLabel,
  cooldownSummary,
  decisionReasonLabel,
  skipClassLabel,
  mutationError,
  nativeModels,
  orderingSignalLabel,
  recoveryReasonLabel,
  refusalAdvice,
  reservationLabel,
  servingHealthLabel,
  tokenCount,
  tokenLimit,
  utcWindowStart,
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
  allowance_charge: null,
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
    (value) =>
      expect(() => nativeModels(value)).toThrow(/native model identifiers/)
  );

  it.each(['valid-model,invalid*', 'invalid*,valid-model'])(
    'rejects a declaration containing an invalid member %s',
    (value) =>
      expect(() => nativeModels(value)).toThrow(/native model identifiers/)
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
    expect(tokenLimit('')).toBeNull();
    expect(tokenLimit(' \n ')).toBeNull();
    expect(tokenLimit('1')).toBe(1);
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
    expect(() => tokenLimit(value)).toThrow(/positive whole numbers/)
  );
});

describe('code diagnostics', () => {
  it.each(['2026-01-01T00:30:00.000Z', '2026-07-01T00:30:00.000Z'])(
    'shows the UTC budget boundary despite the local timezone %s',
    (start) => {
      const label = utcWindowStart(start);
      expect(label).toMatch(/\bUTC$/);
      expect(new Date(label).toISOString()).toBe(start);
    }
  );

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
    expect(usageCertaintyLabel({ ...attempt, nonexecution: true })).toBe(
      'Unknown consumption'
    );
    expect(
      usageCertaintyLabel({
        ...attempt,
        nonexecution: true,
        reported_tokens: 12
      })
    ).toBe('Reported total');
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
    expect(adapterLabel(undefined)).toBe('No adapter');
    expect(adapterLabel('')).toBe('No adapter');
    expect(adapterLabel('fixture-adapter')).toBe('fixture-adapter');
    expect(clientLabel('fixture-client')).toBe('fixture-client');
  });

  it('names the failure class and scope behind a live cooldown', () => {
    expect(cooldownClassLabel('quota')).toBe('Quota');
    expect(cooldownClassLabel('capacity')).toBe('Capacity');
    expect(
      cooldownSummary({
        class: 'quota',
        model: null,
        retry_at: '2026-11-05T12:00:00Z'
      })
    ).toBe('Quota · All models');
    expect(
      cooldownSummary({
        class: 'capacity',
        model: 'fixture-model',
        retry_at: '2026-11-05T12:00:00Z'
      })
    ).toBe('Capacity · fixture-model');
  });

  it('uses operator-facing names for supported subscription families and clients', () => {
    expect(adapterLabel('codex')).toBe('Codex');
    expect(adapterLabel('antigravity')).toBe('Antigravity');
    expect(adapterLabel('zai_coding')).toBe('GLM Coding Plan');
    expect(clientLabel('codex')).toBe('Codex CLI');
    expect(clientLabel('claude-code')).toBe('Claude Code');
    expect(clientLabel('opencode')).toBe('OpenCode');
  });

  it.each([
    'code_invalid_attribution',
    'code_pinned_attribution_override',
    'code_attribution_changed'
  ])('keeps %s focused on repairing attribution', (code) => {
    expect(refusalAdvice(code)).toBe(refusalAdvice('code_missing_attribution'));
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
    expect(refusalAdvice('code_workload_changed')).toContain(
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
    expect(refusalAdvice('code_operation_invalid')).toContain(
      'native model permissions'
    );
    expect(refusalAdvice('code_protocol_unsupported')).toContain(
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
      errors: {
        name: [{ code: 'invalid', message: 'Pick an unused name' }],
        models: [{ code: 'invalid', message: 'Choose a qualified model' }]
      }
    });
    expect(mutationError(fields)).toBe(
      'name: Pick an unused name models: Choose a qualified model'
    );
    expect(mutationError(new Error('Fixture failure'))).toBe('Fixture failure');
    const stale = new ApiProblem({
      type: 'https://openllmproxy.dev/problems/etag_mismatch',
      title: 'Resource changed',
      status: 412
    });
    expect(mutationError(stale)).toContain('Your edits have been kept');
    expect(mutationError(stale)).toContain('latest version');
  });

  it('retains the problem summary when field feedback is empty', () => {
    expect(
      mutationError(
        new ApiProblem({ title: 'Admission denied', status: 403, errors: {} })
      )
    ).toBe('Admission denied');
  });

  it.each([null, undefined])('explains missing failure detail %s', (error) => {
    expect(mutationError(error)).toContain('control API');
  });
});

describe('allowance window presentation', () => {
  const percentWindow = {
    limit_id: 'codex',
    window: 'primary' as const,
    unit: 'percent' as const,
    evidence: 'passive' as const,
    used_percent: 25,
    remaining_percent: 75,
    used_value: null,
    remaining_value: null,
    window_minutes: 300,
    resets_at: '2026-11-05T15:00:00Z',
    observed_at: '2026-11-05T10:00:00Z'
  };

  it('shows percent windows as percentages', () => {
    expect(allowanceWindowSummary(percentWindow)).toBe(
      '25% used · 75% remaining'
    );
  });

  it('shows non-percent windows in the vendor unit', () => {
    expect(
      allowanceWindowSummary({
        ...percentWindow,
        window: 'monthly',
        unit: 'cents',
        evidence: 'active',
        used_percent: 0,
        remaining_percent: 0,
        used_value: 120,
        remaining_value: 880
      })
    ).toBe('120 cents used · 880 cents remaining');
  });

  it('shows unknown rather than zero for unreported values', () => {
    expect(
      allowanceWindowSummary({
        ...percentWindow,
        window: 'weekly',
        unit: 'count',
        used_percent: 0,
        remaining_percent: 0,
        used_value: null,
        remaining_value: null
      })
    ).toBe('Unknown used · Unknown remaining');
  });

  it('labels the evidence kind', () => {
    expect(allowanceEvidenceLabel('passive')).toBe('Passive observation');
    expect(allowanceEvidenceLabel('active')).toBe('Active read');
  });
});

describe('code account decisions', () => {
  it('labels why the chosen account served', () => {
    expect(decisionReasonLabel('chosen')).toBe('Weighted selection');
    expect(decisionReasonLabel('pin')).toBe('Conversation pin');
    expect(decisionReasonLabel('connection')).toBe('Tree connection account');
  });

  it('labels every skip class', () => {
    expect(skipClassLabel('cooldown')).toBe('in cooldown');
    expect(skipClassLabel('unavailable')).toBe('allowance exhausted');
    expect(skipClassLabel('tree')).toBe('tree account served');
    expect(skipClassLabel('weight')).toBe('weighted score');
    expect(skipClassLabel('band')).toBe('lower allowance band');
  });
});

describe('code ordering signals', () => {
  it('names only codex as a proven passive allowance signal', () => {
    expect(orderingSignalLabel('codex')).toBe(
      'Proven passive allowance windows'
    );
    expect(orderingSignalLabel('codex', true)).toBe(
      'Allowance bands from proven passive windows'
    );
    for (const adapter of [
      'zai_coding',
      'minimax_plan',
      'kimi_code',
      'qwen_token_plan',
      'opencode_go',
      'claude',
      'antigravity'
    ]) {
      expect(orderingSignalLabel(adapter, true)).toBe(
        'Weights plus cooldown, no proven allowance signal'
      );
    }
    expect(orderingSignalLabel(null, true)).toBe(
      'Weights plus cooldown, no proven allowance signal'
    );
  });

  it('keeps every adapter on weights plus cooldown while ordering is off', () => {
    expect(orderingSignalLabel('codex', false)).toBe('Weights plus cooldown');
    expect(orderingSignalLabel('zai_coding', false)).toBe(
      'Weights plus cooldown'
    );
  });
});

describe('observed allowance charge presentation', () => {
  it('keeps a missing charge unknown instead of zero', () => {
    const summary = allowanceChargeSummary(attempt);
    expect(summary).toContain('Unknown');
    expect(summary).not.toMatch(/\$0/);
  });

  it('labels the reported allowance as an observed charge in dollars of monthly limit', () => {
    const summary = allowanceChargeSummary({
      ...attempt,
      allowance_charge: { billing_source: 'go', allowance: 0.0021 }
    });
    expect(summary).toBe(
      'Observed charge $0.0021 of monthly limit, billed from go'
    );
    expect(summary).not.toMatch(/remaining/i);
  });

  it('invents no amount when the reported object has no allowance value', () => {
    const summary = allowanceChargeSummary({
      ...attempt,
      allowance_charge: { billing_source: 'balance', list_cost: 0.00412 }
    });
    expect(summary).not.toMatch(/\$/);
    expect(summary).not.toMatch(/remaining/i);
  });
});
