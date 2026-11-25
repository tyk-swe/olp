import { createQuery, skipToken, useQueryClient } from '@tanstack/svelte-query';
import { onDestroy } from 'svelte';
import { overviewKeys } from '#lib/features/overview/index.js';
import { emptyCursorHistory, resetCursor } from '#lib/listing/pagination.js';
import {
  certifyProviderModel,
  declareProviderModels,
  discoverProviderModels,
  getProviderCapabilityOptions,
  listProviderModelPage,
  setProviderModel,
  type CapabilityCertification,
  type CapabilityDeclaration
} from './models';
import {
  certificationPrerequisiteReady,
  parseManualModelNames
} from './providerEditor';
import { providerKeys } from './providerKeys';
import { probeProvider, type Provider } from './providers';
import type { ProviderWizardState } from './providerWizard.svelte';

export class ProviderWizardModelStage {
  private readonly queryClient = useQueryClient();
  readonly pagination = $state(emptyCursorHistory());
  manualModelNames = $state('');
  certificationResults = $state<Record<string, CapabilityCertification>>({});
  reloadVersion = $state(0);
  readonly page;
  readonly capabilityOptions;

  constructor(private readonly wizard: ProviderWizardState) {
    this.page = createQuery(() => ({
      queryKey: providerKeys.models(wizard.providerId, this.pagination.cursor),
      queryFn: ({ signal }) =>
        listProviderModelPage(
          wizard.providerId,
          this.pagination.cursor,
          signal
        ),
      enabled: Boolean(wizard.providerId)
    }));
    this.capabilityOptions = createQuery(() => {
      const kind = this.page.data?.provider.configuration.kind;
      return {
        queryKey: providerKeys.capabilityOptions(kind ?? ''),
        queryFn: kind
          ? ({ signal }: { signal: AbortSignal }) =>
              getProviderCapabilityOptions(kind, signal)
          : skipToken
      };
    });
    onDestroy(() => {
      void this.queryClient.cancelQueries({
        queryKey: providerKeys.modelsOf(wizard.providerId)
      });
    });
  }

  refetch = async () => {
    const id = this.wizard.providerId;
    const cursor = this.pagination.cursor;
    return this.queryClient.fetchQuery({
      queryKey: providerKeys.models(id, cursor),
      queryFn: ({ signal }) => listProviderModelPage(id, cursor, signal),
      staleTime: 0
    });
  };

  reset() {
    this.manualModelNames = '';
    this.certificationResults = {};
    resetCursor(this.pagination);
  }

  private clearCertificationResults() {
    this.wizard.activation.reset();
    this.certificationResults = {};
  }

  private refreshCatalog = () =>
    this.queryClient.invalidateQueries({ queryKey: providerKeys.modelCatalog });

  discover = async () => {
    const provider = this.wizard.wizardProvider;
    if (!provider || !this.wizard.writable) return;
    await this.wizard.action.run(
      async (current) => {
        const discovered = await discoverProviderModels(provider);
        if (!current()) return;
        if (discovered.model_count === 0) {
          throw new Error(
            discovered.configuration.kind === 'openai_compatible'
              ? 'The endpoint returned no models. Use the manual identifier fallback below if it has no model-list API.'
              : 'The upstream returned no models. Verify its identity and cloud context, then retry discovery.'
          );
        }
        this.clearCertificationResults();
        resetCursor(this.pagination);
        await this.refetch();
        if (!current()) return;
        this.wizard.wizardStep = 2;
        await this.refreshCatalog();
      },
      undefined,
      { key: 'discover' }
    );
  };

  declare = async () => {
    const provider = this.wizard.wizardProvider;
    if (!provider) return;
    const names = parseManualModelNames(this.manualModelNames);
    if (!names.length) {
      this.wizard.action.error =
        'Enter at least one upstream model identifier.';
      return;
    }
    if (!this.wizard.writable) return;
    await this.wizard.action.run(
      async (current) => {
        await declareProviderModels(provider, names);
        if (!current()) return;
        this.clearCertificationResults();
        this.manualModelNames = '';
        resetCursor(this.pagination);
        await this.refetch();
        if (!current()) return;
        this.wizard.wizardStep = 2;
        await this.refreshCatalog();
      },
      undefined,
      { key: 'declare-models' }
    );
  };

  review = async (
    modelId: string,
    enabled: boolean,
    capabilities: CapabilityDeclaration[],
    providerEtag: string
  ): Promise<boolean> => {
    const provider = this.wizard.wizardProvider;
    if (!provider || !this.wizard.writable) return false;
    return this.wizard.action.run(
      async (current) => {
        await setProviderModel(
          { ...provider, etag: providerEtag },
          modelId,
          enabled,
          capabilities
        );
        if (!current()) return;
        this.clearCertificationResults();
        await this.refetch();
        if (!current()) return;
        await this.refreshCatalog();
        if (!current()) return;
        await this.queryClient.invalidateQueries({
          queryKey: overviewKeys.root
        });
        if (!current()) return;
        this.wizard.notice =
          'Capability review saved with declared provenance.';
      },
      undefined,
      { key: `model-${modelId}` }
    );
  };

  private certifiable = async (provider: Provider, current: () => boolean) => {
    if (certificationPrerequisiteReady(provider)) return provider;
    const { result: probe } = await probeProvider(provider);
    if (!current()) return undefined;
    this.wizard.probe = probe;
    if (!probe.succeeded) throw new Error(probe.detail);
    const refreshed = await this.refetch();
    return current() ? refreshed.provider : undefined;
  };

  certify = async (modelId: string) => {
    const saved = this.wizard.wizardProvider;
    if (!saved || !this.wizard.writable) return;
    await this.wizard.action.run(
      async (current) => {
        const provider = await this.certifiable(saved, current);
        if (!provider) return;
        const { result } = await certifyProviderModel(provider, modelId);
        if (!current()) return;
        this.certificationResults = {
          ...this.certificationResults,
          [modelId]: result
        };
        await this.refetch();
        if (!current()) return;
        await this.refreshCatalog();
        if (!current()) return;
        this.wizard.probe = null;
        this.wizard.notice = `${result.certified_count} of ${result.attempted_count} reviewed tuples passed server certification. Test the completed draft before activation.`;
      },
      undefined,
      { key: `certify-${modelId}` }
    );
  };
}
