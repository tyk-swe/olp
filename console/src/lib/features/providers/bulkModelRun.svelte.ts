import { untrack } from 'svelte';
import { usePrincipalLifetime } from '#lib/editing/mutationIntent.svelte.js';
import { readResult } from '#lib/editing/result.js';
import { errorMessage } from '#lib/management/http.js';
import type { Provider } from '#lib/domain/providers/api.js';
import {
  planBulkValidation,
  providerTarget,
  type ReviewedInventory
} from './bulkModelValidation.js';
import { useBulkModelReport } from './bulkModelReport.svelte';
import {
  CONNECTION_CHANGED,
  useModelChecks,
  type BulkRun
} from './bulkModelChecks.svelte';

type BulkModelSource = {
  provider: () => Provider;
  canManage: () => boolean;
  inventory: () => ReviewedInventory | undefined;
  reload: (isCurrent: () => boolean) => Promise<void>;
};

function useSelection() {
  let models = $state<string[]>([]);
  let operation = $state('configured');
  usePrincipalLifetime(() => {
    models = [];
  });
  return {
    get models() {
      return models;
    },
    set models(value: string[]) {
      models = value;
    },
    get operation() {
      return operation;
    },
    set operation(value: string) {
      operation = value;
    }
  };
}

function useRunSlot(scope: () => string, onRetire: () => void) {
  let busy = $state(false);
  let held: AbortController | undefined;
  const key = $derived(scope());
  let previousKey = untrack(() => key);
  function retire() {
    held?.abort();
    held = undefined;
    busy = false;
    onRetire();
  }
  usePrincipalLifetime(retire);
  $effect(() => {
    const next = key;
    if (next !== previousKey) {
      previousKey = next;
      untrack(retire);
    }
  });
  return {
    get busy() {
      return busy;
    },
    retireForCurrentScope() {
      retire();
      previousKey = key;
    },
    stop() {
      held?.abort();
    },
    begin() {
      const controller = new AbortController();
      const submittedKey = key;
      held = controller;
      busy = true;
      const isCurrent = () => held === controller && key === submittedKey;
      return {
        signal: controller.signal,
        isCurrent,
        canContinue: () => isCurrent() && !controller.signal.aborted,
        stop: () => controller.abort(),
        finish: () => {
          if (isCurrent()) busy = false;
        }
      };
    }
  };
}

export function useBulkModelRun(source: BulkModelSource) {
  const selection = useSelection();
  const report = useBulkModelReport();
  const checkModels = useModelChecks(report);
  const slot = useRunSlot(
    () =>
      JSON.stringify([
        providerTarget(source.provider()),
        [...selection.models].sort(),
        selection.operation,
        source.canManage()
      ]),
    report.reset
  );
  async function refresh(isCurrent: () => boolean) {
    await source.reload(isCurrent);
    if (isCurrent()) report.refreshed();
  }
  async function check(run: BulkRun, inventory: ReviewedInventory) {
    const plan = await readResult(() =>
      planBulkValidation({
        inventory,
        selected: selection.models,
        operation: selection.operation,
        signal: run.signal
      })
    );
    if (!plan.ok) {
      if (run.isCurrent()) report.addProblem(plan.error);
      return 0;
    }
    const checked = await checkModels(run, inventory.provider, plan.value);
    if (!run.canContinue()) return checked;
    report.announce(
      `Checked ${checked} models. Review the results before activation.`
    );
    const refreshed = await readResult(() => refresh(run.isCurrent));
    if (!refreshed.ok && run.isCurrent()) report.addProblem(refreshed.error);
    return checked;
  }
  async function validate() {
    if (slot.busy || report.refreshPending || !source.canManage()) return;
    if (!selection.models.length) return;
    const inventory = source.inventory();
    if (
      !inventory ||
      providerTarget(inventory.provider) !== providerTarget(source.provider())
    ) {
      report.requireRefresh();
      report.replaceFailures([{ id: null, message: CONNECTION_CHANGED }]);
      return;
    }
    const run = slot.begin();
    report.replaceFailures([]);
    const checked = await check(run, inventory);
    if (run.isCurrent() && run.signal.aborted)
      report.announce(
        `Stopped after ${checked} models. Review the results before activation.`
      );
    run.finish();
  }
  return {
    selection,
    report,
    get busy() {
      return slot.busy;
    },
    validate,
    stop: slot.stop,
    retryFailed() {
      selection.models = [...report.failedIds];
      slot.retireForCurrentScope();
      void validate();
    },
    async retryRefresh() {
      if (slot.busy || !report.refreshPending || !source.canManage()) return;
      const run = slot.begin();
      const refreshed = await readResult(() => refresh(run.isCurrent));
      if (!refreshed.ok && run.isCurrent())
        report.replaceFailures([
          { id: null, message: errorMessage(refreshed.error) }
        ]);
      run.finish();
    }
  };
}
