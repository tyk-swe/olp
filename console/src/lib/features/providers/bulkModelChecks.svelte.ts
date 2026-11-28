import { useAction } from '#lib/editing/action.svelte.js';
import { errorMessage, serializeIfMatch } from '#lib/management/http.js';
import type { Provider } from '#lib/domain/providers/api.js';
import {
  leavesOutcomeUnknown,
  providerTarget,
  validateReviewedModel,
  type PlannedValidation,
  type ReviewedValidationState
} from './bulkModelValidation.js';
import type { BulkModelReport } from './bulkModelReport.svelte';

export const CONNECTION_CHANGED =
  'The connection changed. Refresh before validating these models.';

export type BulkRun = {
  signal: AbortSignal;
  isCurrent: () => boolean;
  canContinue: () => boolean;
  stop: () => void;
};

export function useModelChecks(report: BulkModelReport) {
  const rowAction = useAction();
  return async function checkModels(
    run: BulkRun,
    reviewed: Provider,
    plan: readonly PlannedValidation[]
  ): Promise<number> {
    const review: ReviewedValidationState = {
      expectedETag: reviewed.etag,
      mutationPending: false
    };
    const halt = () => {
      run.stop();
      report.requireRefresh();
    };
    const reviewedMatch = (current: Provider) => {
      if (
        providerTarget(current) === providerTarget(reviewed) &&
        serializeIfMatch(current.etag) === serializeIfMatch(review.expectedETag)
      )
        return true;
      halt();
      report.add({ id: null, message: CONNECTION_CHANGED });
      return false;
    };
    for (const [index, { model, capabilities }] of plan.entries()) {
      if (!run.canContinue()) return index;
      review.mutationPending = false;
      report.announce(
        `Validating ${model.name} (${index + 1}/${plan.length})…`
      );
      await rowAction.run(
        () =>
          validateReviewedModel({
            providerId: reviewed.id,
            model,
            capabilities,
            reviewedMatch,
            state: review,
            signal: run.signal,
            canContinue: run.canContinue,
            isCurrent: run.isCurrent,
            onCommitted: report.requireRefresh
          }),
        (outcome) => {
          if (outcome.status === 'checked' && !outcome.certified)
            report.add({
              id: model.id,
              message: `${model.name}: one or more capabilities failed validation.`
            });
        },
        {
          key: model.id,
          failure: (error) => {
            if (!run.canContinue()) return '';
            report.add({
              id: model.id,
              message: `${model.name}: ${errorMessage(error)}`
            });
            if (review.mutationPending && leavesOutcomeUnknown(error)) halt();
            return errorMessage(error);
          }
        }
      );
      if (!run.canContinue()) return index;
    }
    return plan.length;
  };
}
