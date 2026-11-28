import { errorMessage } from '#lib/management/http.js';

export type BulkFailure = { id: string | null; message: string };

export function useBulkModelReport() {
  let status = $state('');
  let refreshPending = $state(false);
  let failures = $state.raw<readonly BulkFailure[]>([]);
  const failedIds = $derived(
    failures.flatMap((failure) => (failure.id ? [failure.id] : []))
  );
  return {
    get status() {
      return status;
    },
    get refreshPending() {
      return refreshPending;
    },
    get failures() {
      return failures;
    },
    get failedIds() {
      return failedIds;
    },
    announce(message: string) {
      status = message;
    },
    requireRefresh() {
      refreshPending = true;
    },
    refreshed() {
      refreshPending = false;
      failures = failures.filter((failure) => failure.id !== null);
    },
    add(failure: BulkFailure) {
      failures = [...failures, failure];
    },
    addProblem(error: unknown) {
      failures = [...failures, { id: null, message: errorMessage(error) }];
    },
    replaceFailures(next: readonly BulkFailure[]) {
      failures = next;
    },
    reset() {
      status = '';
      refreshPending = false;
      failures = [];
    }
  };
}

export type BulkModelReport = ReturnType<typeof useBulkModelReport>;
