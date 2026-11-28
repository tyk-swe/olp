import { useRole } from '#lib/session/useRole.svelte.js';
import { useWizardActivation } from './activation.svelte';
import { useWizardConnection } from './connection.svelte';
import { useConnectionDraft } from './connectionDraft.svelte';
import { useWizardModels } from './models.svelte';
import { useWizardOperation } from './operation.svelte';
import { useSavedProvider } from './savedProvider.svelte';

export type WizardStep = 1 | 2 | 3;

export function useProviderWizard(copyFrom: string | null) {
  const access = useRole();
  const canManage = $derived(access.can('providers.manage'));
  let step = $state<WizardStep>(1);
  let reviewVersion = $state(0);
  const showStep = (next: WizardStep) => {
    step = next;
  };
  const operation = useWizardOperation(
    () => canManage && !connection.grant.busy
  );
  const saved = useSavedProvider();
  const activation = useWizardActivation({ operation, saved, showStep });
  const form = useConnectionDraft(
    operation,
    copyFrom,
    () => !saved.id && !connection.started
  );
  const connection = useWizardConnection({
    operation,
    saved,
    form,
    canManage: () => canManage,
    retireActivation: activation.retire,
    showStep
  });
  const models = useWizardModels({
    operation,
    saved,
    retireActivation: activation.retire,
    showStep
  });
  const busy = $derived(operation.busy || connection.grant.busy);
  return {
    operation,
    saved,
    form,
    connection,
    models,
    activation,
    get canManage() {
      return canManage;
    },
    get busy() {
      return busy;
    },
    get step() {
      return step;
    },
    get reviewVersion() {
      return reviewVersion;
    },
    async reload() {
      if (!saved.provider) return;
      await operation.run('reload', async (isCurrent) => {
        await saved.refetch();
        if (!isCurrent()) return;
        reviewVersion += 1;
        operation.resolveConflict();
      });
    },
    continueToActivation() {
      step = 3;
    },
    goBack() {
      if (busy || step === 1) return;
      if (activation.unresolved)
        return operation.report(
          'Activation may already have committed. Retry activation before changing this connection, or leave the wizard.'
        );
      step = step === 3 ? 2 : 1;
      if (step === 1 && saved.provider) form.restore(saved.provider);
    }
  };
}
