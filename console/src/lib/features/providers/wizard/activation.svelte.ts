import { useQueryClient } from '@tanstack/svelte-query';
import {
  useMutationIntent,
  usePrincipalLifetime
} from '#lib/editing/mutationIntent.svelte.js';
import { refreshProviderReceipt } from '../providerChanges';
import { probeSummary } from '../providerReadiness';
import {
  commitProviderActivation,
  type ActivationProgress
} from './connectionProgress';
import type { WizardOperation } from './operation.svelte';
import type { SavedProvider } from './savedProvider.svelte';

type Dependencies = {
  operation: WizardOperation;
  saved: SavedProvider;
  showStep: (step: 3) => void;
};

export function useWizardActivation(dependencies: Dependencies) {
  const { operation, saved, showStep } = dependencies;
  const queryClient = useQueryClient();
  const intent = useMutationIntent();
  let progress: ActivationProgress | undefined;
  usePrincipalLifetime(() => {
    progress = undefined;
  });
  return {
    get unresolved() {
      return intent.unresolved;
    },
    retire() {
      progress = undefined;
      intent.reset();
    },
    async test() {
      const provider = saved.provider;
      if (!provider) return;
      await operation.run('final-probe', async (isCurrent) => {
        await saved.test(provider, isCurrent);
        if (!isCurrent() || !saved.probe) return;
        const summary = probeSummary(saved.probe);
        await saved.refetch();
        if (isCurrent())
          operation.announce(`Final draft test passed: ${summary}`);
      });
    },
    async activate() {
      const provider = saved.provider;
      if (!provider) return;
      const attempt: ActivationProgress = progress ?? {
        input: { id: provider.id, etag: provider.etag },
        phase: 'pending',
        uncertain: false
      };
      progress = attempt;
      await operation.run(
        'activate',
        (isCurrent) =>
          commitProviderActivation(attempt, {
            activateIntent: intent,
            isCurrent,
            setProgress: (next) => {
              progress = next;
            }
          }),
        {
          accept: async (committed, isCurrent) => {
            if (!committed) return;
            const notice = `Provider activated in runtime generation ${committed.generation}.`;
            operation.announce(notice);
            const refreshed = await refreshProviderReceipt(
              queryClient,
              { change: { kind: 'activation', id: provider.id }, notice },
              async () => {
                await saved.refetch();
                if (isCurrent()) showStep(3);
              },
              isCurrent
            );
            if (refreshed.kind === 'refresh-failed')
              operation.reportRefreshFailure(refreshed.message);
          }
        }
      );
    }
  };
}
