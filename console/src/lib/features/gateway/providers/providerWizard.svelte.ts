import { queryKeys } from '$lib/api/queryKeys';
import { createQuery, useQueryClient } from '@tanstack/svelte-query';
import { onDestroy } from 'svelte';
import {
  errorMessage as message,
  fieldIssues,
  isEtagMismatch,
  type FieldIssue
} from '$lib/api/http';
import { emptyCursorHistory, resetCursor } from '$lib/lists/pagination';
import {
  activateProvider,
  createProvider,
  getProvider,
  probeProvider,
  updateProvider,
  type Provider,
  type ProviderProbe
} from '$lib/api/management/providers';
import {
  certifyProviderModel,
  declareProviderModels,
  discoverProviderModels,
  getProviderCapabilityOptions,
  listProviderKinds,
  listProviderModelPage,
  setProviderModel,
  type CapabilityDeclaration,
  type CapabilityCertification
} from '$lib/api/management/providerModels';
import { rotateProviderCredential } from '$lib/api/management/providerCredentials';
import {
  authOptionsFor,
  buildCreateProviderInput,
  buildUpdateProviderInput,
  certificationPrerequisiteReady,
  createProviderDraft,
  parseManualModelNames,
  probeSummary,
  requiresCredential,
  validateProviderDraft,
  type ProviderDraft
} from './providerEditor';
import { useRole } from '$lib/auth/useRole.svelte';

export class ProviderWizardState {
  stepLabels = [
    'Connector',
    'Test',
    'Discovery',
    'Capabilities',
    'Activate'
  ] as const;
  access = useRole();
  canManage = $derived(this.access.can('providers.manage'));
  queryClient = useQueryClient();
  providerKinds;
  draft = $state<ProviderDraft | null>(null);
  wizardProvider = $state<Provider | null>(null);
  wizardStep = $state(1);
  wizardModelPagination = $state(emptyCursorHistory());
  wizardModels;
  capabilityOptions;
  probe = $state<ProviderProbe | null>(null);
  manualModelNames = $state('');
  busy = $state('');
  errorMessage = $state('');
  validationIssues = $state<FieldIssue[]>([]);
  notice = $state('');
  certificationResults = $state<Record<string, CapabilityCertification>>({});
  wizardConflict = $state(false);
  wizardModelReloadVersion = $state(0);
  selectedSpec = $derived.by(() =>
    this.providerKinds.data?.find(
      (candidate) => candidate.kind === this.draft?.kind
    )
  );
  authOptions = $derived(
    this.selectedSpec ? authOptionsFor(this.selectedSpec) : []
  );
  credentialRequired = $derived(
    Boolean(
      this.draft &&
      this.selectedSpec &&
      requiresCredential(this.selectedSpec, this.draft.authMode)
    )
  );
  run = async (
    label: string,
    action: () => Promise<void>
  ): Promise<boolean> => {
    this.busy = label;
    this.errorMessage = '';
    this.validationIssues = [];
    this.notice = '';
    try {
      await action();
      return true;
    } catch (error) {
      if (this.wizardProvider && isEtagMismatch(error))
        this.wizardConflict = true;
      else {
        this.errorMessage = message(error);
        this.validationIssues = fieldIssues(error);
      }
      return false;
    } finally {
      this.busy = '';
    }
  };
  clearCertificationResults = () => {
    this.certificationResults = {};
  };
  refetchWizardModels = async () => {
    const result = await this.wizardModels.refetch();
    if (result.error) throw result.error;
    if (!result.data)
      throw new Error('The provider model reload returned no data.');
    return result.data;
  };
  reloadWizard = async () => {
    if (!this.wizardProvider) return;
    this.busy = 'reload';
    this.errorMessage = '';
    this.validationIssues = [];
    this.notice = '';
    try {
      const reloadedProvider = await getProvider(this.wizardProvider.id);
      await this.refetchWizardModels();
      this.wizardProvider = reloadedProvider;
      this.wizardModelReloadVersion += 1;
      this.wizardConflict = false;
    } catch (error) {
      this.errorMessage = message(error);
    } finally {
      this.busy = '';
    }
  };
  goBack = () => {
    if (this.wizardStep > 1) this.wizardStep -= 1;
  };
  createDraft = async (event: SubmitEvent) => {
    event.preventDefault();
    if (!this.draft || !this.selectedSpec) return;
    const current = this.draft;
    const spec = this.selectedSpec;
    // The connector kind is locked once the draft exists, so stepping back
    // always edits that provider rather than orphaning it behind a second one.
    const existing = this.wizardProvider;
    const issue = validateProviderDraft(current, spec, {
      // The first pass stored a write-only credential; the field is cleared
      // afterwards and must not be demanded again on a re-save.
      credentialAlreadyStored: Boolean(existing)
    });
    if (issue) {
      this.errorMessage = issue;
      this.validationIssues = [];
      return;
    }
    await this.run('create', async () => {
      let id: string;
      if (existing) {
        const updated = await updateProvider(
          existing.id,
          existing.etag,
          buildUpdateProviderInput(
            {
              name: current.name,
              endpoint: current.endpoint,
              apiVersion: current.apiVersion,
              cloudRegion: current.cloudRegion,
              cloudProject: current.cloudProject,
              deployment: current.deployment,
              authMode: current.authMode
            },
            spec
          )
        );
        if (current.credential) {
          await rotateProviderCredential(updated, current.credential);
        }
        id = updated.id;
      } else {
        id = await createProvider(buildCreateProviderInput(current, spec));
      }
      current.credential = '';
      this.wizardProvider = await getProvider(id);
      this.wizardStep = 2;
      await Promise.all([
        this.queryClient.invalidateQueries({
          queryKey: queryKeys.providers.summaries
        }),
        this.queryClient.invalidateQueries({
          queryKey: queryKeys.providers.modelCatalog
        })
      ]);
    });
  };
  testWizardProvider = async () => {
    if (!this.wizardProvider) return;
    await this.run('probe', async () => {
      this.probe = await probeProvider(this.wizardProvider!);
      if (!this.probe.succeeded) throw new Error(this.probe.detail);
      this.wizardStep = 3;
    });
  };
  discoverWizardProvider = async () => {
    if (!this.wizardProvider) return;
    await this.run('discover', async () => {
      const discovered = await discoverProviderModels(this.wizardProvider!);
      if (discovered.model_count === 0) {
        throw new Error(
          discovered.kind === 'openai_compatible'
            ? 'The endpoint returned no models. Use the manual identifier fallback below if it has no model-list API.'
            : 'The upstream returned no models. Verify its identity and cloud context, then retry discovery.'
        );
      }
      this.clearCertificationResults();
      resetCursor(this.wizardModelPagination);
      await this.refetchWizardModels();
      this.wizardProvider = discovered;
      this.wizardStep = 4;
      await this.queryClient.invalidateQueries({
        queryKey: queryKeys.providers.modelCatalog
      });
    });
  };
  declareWizardModels = async () => {
    if (!this.wizardProvider) return;
    const names = parseManualModelNames(this.manualModelNames);
    if (!names.length) {
      this.errorMessage = 'Enter at least one upstream model identifier.';
      return;
    }
    await this.run('declare-models', async () => {
      const declared = await declareProviderModels(this.wizardProvider!, names);
      this.clearCertificationResults();
      this.manualModelNames = '';
      resetCursor(this.wizardModelPagination);
      await this.refetchWizardModels();
      this.wizardProvider = declared;
      this.wizardStep = 4;
      await this.queryClient.invalidateQueries({
        queryKey: queryKeys.providers.modelCatalog
      });
    });
  };
  reviewWizardModel = async (
    modelId: string,
    enabled: boolean,
    capabilities: CapabilityDeclaration[],
    providerEtag: string
  ): Promise<boolean> => {
    if (!this.wizardProvider) return false;
    return this.run(`model-${modelId}`, async () => {
      const updated = await setProviderModel(
        { ...this.wizardProvider!, etag: providerEtag },
        modelId,
        enabled,
        capabilities
      );
      this.clearCertificationResults();
      await this.refetchWizardModels();
      this.wizardProvider = updated;
      await this.queryClient.invalidateQueries({
        queryKey: queryKeys.providers.modelCatalog
      });
      this.notice = 'Capability review saved with declared provenance.';
    });
  };
  certifyWizardModel = async (modelId: string) => {
    if (!this.wizardProvider) return;
    await this.run(`certify-${modelId}`, async () => {
      if (!certificationPrerequisiteReady(this.wizardProvider!)) {
        this.probe = await probeProvider(this.wizardProvider!);
        if (!this.probe.succeeded) throw new Error(this.probe.detail);
        this.wizardProvider = {
          ...this.wizardProvider!,
          last_probe_at: this.probe.checked_at,
          last_probe_status: 'succeeded',
          last_probe_detail: this.probe.detail
        };
      }
      const result = await certifyProviderModel(this.wizardProvider!, modelId);
      this.certificationResults = {
        ...this.certificationResults,
        [modelId]: result
      };
      const updated = await getProvider(this.wizardProvider!.id);
      await this.refetchWizardModels();
      this.wizardProvider = updated;
      await this.queryClient.invalidateQueries({
        queryKey: queryKeys.providers.modelCatalog
      });
      this.probe = null;
      this.notice = `${result.certified_count} of ${result.attempted_count} reviewed tuples passed server certification. Test the completed draft before activation.`;
    });
  };
  testWizardDraftForActivation = async () => {
    if (!this.wizardProvider) return;
    await this.run('final-probe', async () => {
      this.probe = await probeProvider(this.wizardProvider!);
      if (!this.probe.succeeded) throw new Error(this.probe.detail);
      this.wizardProvider = await getProvider(this.wizardProvider!.id);
      this.notice = `Final draft test passed: ${probeSummary(this.probe)}`;
    });
  };
  activateWizardProvider = async () => {
    if (!this.wizardProvider) return;
    await this.run('activate', async () => {
      const generation = await activateProvider(this.wizardProvider!);
      this.wizardProvider = await getProvider(this.wizardProvider!.id);
      this.wizardStep = 5;
      this.notice = `Provider activated in runtime generation ${generation}.`;
      await Promise.all([
        this.queryClient.invalidateQueries({
          queryKey: queryKeys.providers.summaries
        }),
        this.queryClient.invalidateQueries({
          queryKey: queryKeys.providers.modelCatalog
        })
      ]);
    });
  };
  constructor() {
    this.providerKinds = createQuery(() => ({
      queryKey: queryKeys.providers.kinds(),
      queryFn: ({ signal }) => listProviderKinds(signal)
    }));
    this.wizardModels = createQuery(() => ({
      queryKey: queryKeys.providers.models(
        this.wizardProvider?.id ?? '',
        this.wizardModelPagination.cursor
      ),
      queryFn: ({ signal }) =>
        listProviderModelPage(
          this.wizardProvider!.id,
          this.wizardModelPagination.cursor,
          signal
        ),
      enabled: Boolean(this.wizardProvider) && this.wizardStep >= 3
    }));
    this.capabilityOptions = createQuery(() => ({
      queryKey: queryKeys.providers.capabilityOptions(
        this.wizardProvider?.kind ?? ''
      ),
      queryFn: ({ signal }) =>
        getProviderCapabilityOptions(this.wizardProvider!.kind, signal),
      enabled: Boolean(this.wizardProvider)
    }));
    $effect(() => {
      const first = this.providerKinds.data?.[0];
      if (!this.draft && first) this.draft = createProviderDraft(first);
      const current = this.draft;
      const spec = this.selectedSpec;
      if (!current || !spec) return;
      if (!this.authOptions.some(([value]) => value === current.authMode)) {
        current.authMode = spec.default_auth_mode;
      }
      if (!this.credentialRequired) current.credential = '';
    });
    onDestroy(() => {
      if (this.draft) this.draft.credential = '';
    });
  }
}
