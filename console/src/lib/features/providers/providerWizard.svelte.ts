import { providerKeys } from '$lib/features/providers/providerKeys';
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
  probeProvider,
  updateProvider,
  type ProviderProbe
} from '$lib/features/providers/api';
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
} from '$lib/features/providers/models';
import { rotateProviderCredential } from '$lib/features/providers/credentials';
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
} from '$lib/features/providers/providerEditor';
import { useRole } from '$lib/features/access/session/useRole.svelte';

export class ProviderWizardState {
  stepLabels = ['Connection', 'Models and capabilities', 'Activation'] as const;
  access = useRole();
  canManage = $derived(this.access.can('providers.manage'));
  queryClient = useQueryClient();
  providerKinds;
  draft = $state<ProviderDraft | null>(null);
  providerId = $state('');
  wizardProvider = $derived.by(() => this.wizardModels.data?.provider ?? null);
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
    const id = this.providerId;
    const cursor = this.wizardModelPagination.cursor;
    return this.queryClient.fetchQuery({
      queryKey: providerKeys.models(id, cursor),
      queryFn: ({ signal }) => listProviderModelPage(id, cursor, signal),
      staleTime: 0
    });
  };
  reloadWizard = async () => {
    if (!this.wizardProvider) return;
    this.busy = 'reload';
    this.errorMessage = '';
    this.validationIssues = [];
    this.notice = '';
    try {
      await this.refetchWizardModels();
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
      this.providerId = id;
      const snapshot = await this.refetchWizardModels();
      await Promise.all([
        this.queryClient.invalidateQueries({
          queryKey: providerKeys.summaries
        }),
        this.queryClient.invalidateQueries({
          queryKey: providerKeys.modelCatalog
        })
      ]);
      this.probe = await probeProvider(snapshot.provider);
      if (!this.probe.succeeded) throw new Error(this.probe.detail);
      this.wizardStep = 2;
    });
  };
  discoverWizardProvider = async () => {
    if (!this.wizardProvider) return;
    await this.run('discover', async () => {
      const discovered = await discoverProviderModels(this.wizardProvider!);
      if (discovered.model_count === 0) {
        throw new Error(
          discovered.configuration.kind === 'openai_compatible'
            ? 'The endpoint returned no models. Use the manual identifier fallback below if it has no model-list API.'
            : 'The upstream returned no models. Verify its identity and cloud context, then retry discovery.'
        );
      }
      this.clearCertificationResults();
      resetCursor(this.wizardModelPagination);
      await this.refetchWizardModels();
      this.wizardStep = 2;
      await this.queryClient.invalidateQueries({
        queryKey: providerKeys.modelCatalog
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
      await declareProviderModels(this.wizardProvider!, names);
      this.clearCertificationResults();
      this.manualModelNames = '';
      resetCursor(this.wizardModelPagination);
      await this.refetchWizardModels();
      this.wizardStep = 2;
      await this.queryClient.invalidateQueries({
        queryKey: providerKeys.modelCatalog
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
      await setProviderModel(
        { ...this.wizardProvider!, etag: providerEtag },
        modelId,
        enabled,
        capabilities
      );
      this.clearCertificationResults();
      await this.refetchWizardModels();
      await this.queryClient.invalidateQueries({
        queryKey: providerKeys.modelCatalog
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
        await this.refetchWizardModels();
      }
      const result = await certifyProviderModel(this.wizardProvider!, modelId);
      this.certificationResults = {
        ...this.certificationResults,
        [modelId]: result
      };
      await this.refetchWizardModels();
      await this.queryClient.invalidateQueries({
        queryKey: providerKeys.modelCatalog
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
      await this.refetchWizardModels();
      this.notice = `Final draft test passed: ${probeSummary(this.probe)}`;
    });
  };
  activateWizardProvider = async () => {
    if (!this.wizardProvider) return;
    await this.run('activate', async () => {
      const generation = await activateProvider(this.wizardProvider!);
      await this.refetchWizardModels();
      this.wizardStep = 3;
      this.notice = `Provider activated in runtime generation ${generation}.`;
      await Promise.all([
        this.queryClient.invalidateQueries({
          queryKey: providerKeys.summaries
        }),
        this.queryClient.invalidateQueries({
          queryKey: providerKeys.modelCatalog
        })
      ]);
    });
  };
  constructor() {
    this.providerKinds = createQuery(() => ({
      queryKey: providerKeys.kinds(),
      queryFn: ({ signal }) => listProviderKinds(signal)
    }));
    this.wizardModels = createQuery(() => ({
      queryKey: providerKeys.models(
        this.providerId,
        this.wizardModelPagination.cursor
      ),
      queryFn: ({ signal }) =>
        listProviderModelPage(
          this.providerId,
          this.wizardModelPagination.cursor,
          signal
        ),
      enabled: Boolean(this.providerId)
    }));
    this.capabilityOptions = createQuery(() => ({
      queryKey: providerKeys.capabilityOptions(
        this.wizardProvider?.configuration.kind ?? ''
      ),
      queryFn: ({ signal }) =>
        getProviderCapabilityOptions(
          this.wizardProvider!.configuration.kind,
          signal
        ),
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
      void this.queryClient.cancelQueries({
        queryKey: providerKeys.modelsOf(this.providerId)
      });
      if (this.draft) this.draft.credential = '';
    });
  }
}
