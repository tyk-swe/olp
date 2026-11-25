import type { QueryClient } from '@tanstack/svelte-query';
import { MutationIntent } from '#lib/editing/mutationIntent.js';
import { refreshProviderReceipt } from './providerChanges';
import { probeSummary } from './providerEditor';
import { probeProvider } from './providers';
import {
  commitProviderActivation,
  type ActivationProgress
} from './providerWizardConnection';
import type { ProviderWizardState } from './providerWizard.svelte';

export class ProviderWizardActivationStage {
  private progress?: ActivationProgress;
  private readonly intent = new MutationIntent();

  constructor(
    private readonly wizard: ProviderWizardState,
    private readonly queryClient: QueryClient
  ) {}

  get unresolved() {
    return this.intent.unresolved;
  }

  reset() {
    this.progress = undefined;
    this.intent.reset();
  }

  test = async () => {
    const provider = this.wizard.wizardProvider;
    if (!provider || !this.wizard.writable) return;
    await this.wizard.action.run(
      async (current) => {
        const { result: probe } = await probeProvider(provider);
        if (!current()) return;
        this.wizard.probe = probe;
        if (!probe.succeeded) throw new Error(probe.detail);
        await this.wizard.models.refetch();
        if (!current()) return;
        this.wizard.notice = `Final draft test passed: ${probeSummary(probe)}`;
      },
      undefined,
      { key: 'final-probe' }
    );
  };

  activate = async () => {
    const provider = this.wizard.wizardProvider;
    if (!provider) return;
    const progress: ActivationProgress = this.progress ?? {
      input: { id: provider.id, etag: provider.etag },
      phase: 'pending',
      uncertain: false
    };
    this.progress = progress;
    if (!this.wizard.writable) return;
    await this.wizard.action.run(
      async (current) => {
        const committed = await commitProviderActivation(progress, {
          activateIntent: this.intent,
          isCurrent: current,
          setProgress: (next) => {
            this.progress = next;
          }
        });
        if (!committed || !current()) return;
        const notice = `Provider activated in runtime generation ${committed.generation}.`;
        this.wizard.notice = notice;
        const refreshed = await refreshProviderReceipt(
          this.queryClient,
          { change: { kind: 'activation', id: provider.id }, notice },
          async () => {
            const snapshot = await this.wizard.models.refetch();
            if (current()) this.wizard.wizardStep = 3;
            return snapshot;
          },
          current
        );
        if (refreshed.kind === 'refresh-failed')
          this.wizard.action.error = `The provider change succeeded, but refreshing failed. ${refreshed.message}`;
      },
      undefined,
      { key: 'activate' }
    );
  };
}
