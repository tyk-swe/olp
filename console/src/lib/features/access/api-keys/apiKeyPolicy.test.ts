import { expect, it } from 'vitest';
import { buildApiKeyPolicyInput, createApiKeyFormState } from './apiKeyPolicy';

it('includes workflow run limits in a key policy', () => {
  const form = {
    ...createApiKeyFormState(null),
    runLimits: {
      max_iterations: 3,
      max_tool_calls: 0,
      max_concurrent_runs: 2,
      max_run_cost: '0.25'
    }
  };
  expect(buildApiKeyPolicyInput(form).run_limits).toEqual(form.runLimits);
});
