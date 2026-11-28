import { useQueryClient } from '@tanstack/svelte-query';
import { overviewKeys } from '#lib/features/overview/index.js';
import { providerKeys } from '#lib/domain/providers/providerKeys.js';
import type { Provider } from '#lib/domain/providers/api.js';
import {
  certifyProviderModel,
  declareProviderModels,
  discoverProviderModels,
  setProviderModel,
  type CapabilityCertification,
  type CapabilityDeclaration
} from '../models';
import { validateModelNames } from '../providerEditor';
import { certificationPrerequisiteReady } from '../providerReadiness';
import type { WizardOperation } from './operation.svelte';
import type { SavedProvider } from './savedProvider.svelte';

type Dependencies = {
  operation: WizardOperation;
  saved: SavedProvider;
  retireActivation: () => void;
  showStep: (step: 2) => void;
};

function emptyDiscovery(provider: Provider) {
  return provider.configuration.kind === 'openai_compatible'
    ? 'The endpoint returned no models. Use the manual identifier fallback below if it has no model-list API.'
    : 'The upstream returned no models. Verify its identity and cloud context, then retry discovery.';
}

async function certifySaved(
  saved: SavedProvider,
  current: Provider,
  modelId: string,
  isCurrent: () => boolean
) {
  const tested = certificationPrerequisiteReady(current);
  if (!tested) await saved.test(current, isCurrent);
  if (!isCurrent()) return;
  const provider = tested ? current : (await saved.refetch()).provider;
  if (!isCurrent()) return;
  return (await certifyProviderModel(provider, modelId)).result;
}

export function useWizardModels(dependencies: Dependencies) {
  const { operation, saved, retireActivation, showStep } = dependencies;
  const queryClient = useQueryClient();
  let manualNames = $state('');
  let results = $state.raw<Record<string, CapabilityCertification>>({});
  const refreshCatalog = () =>
    queryClient.invalidateQueries({ queryKey: providerKeys.modelCatalog });
  function changed() {
    retireActivation();
    results = {};
  }
  async function showInventory(isCurrent: () => boolean) {
    saved.firstPage();
    await saved.refetch();
    if (!isCurrent()) return;
    showStep(2);
    await refreshCatalog();
  }
  return {
    get manualNames() {
      return manualNames;
    },
    set manualNames(value: string) {
      manualNames = value;
    },
    get certificationResults() {
      return results;
    },
    async discover() {
      const provider = saved.provider;
      if (!provider) return;
      await operation.run('discover', async (isCurrent) => {
        const discovered = await discoverProviderModels(provider);
        if (!isCurrent()) return;
        if (discovered.model_count === 0)
          throw new Error(emptyDiscovery(discovered));
        changed();
        await showInventory(isCurrent);
      });
    },
    async declare() {
      const provider = saved.provider;
      if (!provider) return;
      const names = validateModelNames(manualNames);
      if (!operation.check(names)) return;
      await operation.run('declare-models', async (isCurrent) => {
        await declareProviderModels(provider, names.value);
        if (!isCurrent()) return;
        changed();
        manualNames = '';
        await showInventory(isCurrent);
      });
    },
    async review(
      modelId: string,
      enabled: boolean,
      capabilities: CapabilityDeclaration[],
      providerEtag: string
    ): Promise<boolean> {
      const provider = saved.provider;
      if (!provider) return false;
      return operation.run(`model-${modelId}`, async (isCurrent) => {
        await setProviderModel(
          { ...provider, etag: providerEtag },
          modelId,
          enabled,
          capabilities
        );
        if (!isCurrent()) return;
        changed();
        await saved.refetch();
        if (!isCurrent()) return;
        await refreshCatalog();
        if (!isCurrent()) return;
        await queryClient.invalidateQueries({ queryKey: overviewKeys.root });
        if (!isCurrent()) return;
        operation.announce('Capability review saved with declared provenance.');
      });
    },
    async certify(modelId: string) {
      const current = saved.provider;
      if (!current) return;
      await operation.run(`certify-${modelId}`, async (isCurrent) => {
        const result = await certifySaved(saved, current, modelId, isCurrent);
        if (!result || !isCurrent()) return;
        results = { ...results, [modelId]: result };
        await saved.refetch();
        if (!isCurrent()) return;
        await refreshCatalog();
        if (!isCurrent()) return;
        saved.forgetProbe();
        operation.announce(
          `${result.certified_count} of ${result.attempted_count} reviewed tuples passed server certification. Test the completed draft before activation.`
        );
      });
    }
  };
}
