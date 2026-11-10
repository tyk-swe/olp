import { overviewKeys } from '#lib/features/overview/overviewKeys.js';
import { snapshotNativeJSON, stringifyNativeJSON } from '#lib/nativeJson.js';
import { networkCredentialPath } from './configurationDraft.svelte';
import { refreshProviderReceipt } from './providerChanges';
import { providerKeys } from '#lib/features/providers/providerKeys.js';
import { createQuery, skipToken, useQueryClient } from '@tanstack/svelte-query';
import { onDestroy } from 'svelte';
import { SvelteURLSearchParams } from 'svelte/reactivity';
import {
  ApiProblem,
  errorMessage,
  fieldIssues,
  isEtagMismatch,
  type FieldIssue
} from '#lib/management/http.js';
import { emptyCursorHistory, resetCursor } from '#lib/listing/pagination.js';
import {
  getProvider,
  activateProvider,
  createProvider,
  probeProvider,
  updateProvider,
  type Provider,
  type ProviderProbe
} from '#lib/features/providers/providers.js';
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
} from '#lib/features/providers/models.js';
import {
  listProviderCredentials,
  rotateProviderCredential,
  type ProviderCredential
} from '#lib/features/providers/credentials.js';
import type { ProviderProfile } from '#lib/features/providers/providerProfiles.js';
import type { ProviderVendor } from '#lib/features/providers/providers.js';
import type { GrantEnrollment } from './grants';
import { GrantEnrollmentState } from './grantEnrollment.svelte';
import {
  authOptionsFor,
  buildCreateProviderInput,
  certificationPrerequisiteReady,
  createProviderDraft,
  parseManualModelNames,
  probeSummary,
  requiresCredential,
  requiresGrant,
  validateProviderDraft,
  type ProviderDraft
} from '#lib/features/providers/providerEditor.js';
import { useRole } from '#lib/features/access/session/useRole.svelte.js';
import {
  useMutationIntent,
  usePrincipalLifetime
} from '#lib/editing/mutationIntent.svelte.js';

function isLiveGrant(
  credentials: ProviderCredential[] | undefined,
  id: string | null | undefined,
  pluginDigest: string | undefined
): boolean {
  const credential = credentials?.find((candidate) => candidate.id === id);
  return Boolean(
    credential?.grant &&
    !credential.revoked_at &&
    !credential.grant.lapsed_at &&
    credential.grant.plugin_digest === pluginDigest
  );
}

type ConnectionInput = Parameters<typeof createProvider>[0];
type ConnectionProgress = { input: ConnectionInput } & (
  | { phase: 'create'; uncertain: boolean }
  | {
      phase: 'update';
      provider: Pick<Provider, 'id' | 'etag'>;
      uncertain: boolean;
    }
  | {
      phase: 'credential';
      provider: Pick<Provider, 'id' | 'etag'>;
      status: 'ready' | 'refused' | 'uncertain';
    }
  | { phase: 'saved'; id: string }
);

type ActivationProgress = { input: Pick<Provider, 'id' | 'etag'> } & (
  | { phase: 'pending'; uncertain: boolean }
  | { phase: 'committed'; generation: number }
);

export class ProviderWizardState {
  private connectionProgress?: ConnectionProgress;
  private activationProgress?: ActivationProgress;
  private ownerEpoch = 0;
  rotateProviderCredentialIntent = useMutationIntent();
  activateProviderIntent = useMutationIntent();

  createIntent = useMutationIntent();

  stepLabels = ['Connection', 'Models and capabilities', 'Activation'] as const;
  access = useRole();
  canManage = $derived(this.access.can('providers.manage'));
  queryClient = useQueryClient();
  providerKinds;
  draft = $state<ProviderDraft | null>(null);
  providerId = $state('');
  copyLoaded = false;
  wizardProvider = $derived.by(() => this.wizardModels.data?.provider ?? null);
  wizardStep = $state(1);
  wizardModelPagination = $state(emptyCursorHistory());
  wizardModels;
  capabilityOptions;

  draftCredentials;
  probe = $state<ProviderProbe | null>(null);

  private grantPrincipal = this.access.user?.id;
  grant = new GrantEnrollmentState({
    allowed: () =>
      this.canManage && this.access.user?.id === this.grantPrincipal,
    error: (error, failure) => {
      if (this.wizardProvider && isEtagMismatch(failure))
        this.wizardConflict = true;
      else {
        this.errorMessage = error;
        this.validationIssues = fieldIssues(failure);
      }
    },
    completed: async (_completion, { isCurrent }) => {
      this.notice =
        'Grant enrolled. Testing the connection remains a separate step.';
      await this.testEnrolledGrant(isCurrent);
    }
  });
  get grantEnrollment(): GrantEnrollment | null {
    return this.grant.enrollment;
  }

  get grantInput() {
    return this.grant.input;
  }
  set grantInput(input: string) {
    this.grant.input = input;
  }
  manualModelNames = $state('');
  private operationBusy = $state('');
  get busy() {
    return this.operationBusy || this.grant.busy;
  }
  set busy(value: string) {
    this.operationBusy = value;
  }
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
  grantRequired = $derived(
    Boolean(
      this.draft &&
      this.selectedSpec &&
      requiresGrant(this.selectedSpec, this.draft.authMode)
    )
  );

  grantEnrolled = $derived.by(() =>
    isLiveGrant(
      this.draftCredentials.data,
      this.wizardProvider?.draft_credential_id,
      this.draft?.profileRevision
    )
  );
  private refused(error: unknown) {
    return (
      error instanceof ApiProblem &&
      error.problem.status >= 400 &&
      error.problem.status < 500 &&
      ![408, 429].includes(error.problem.status)
    );
  }
  private retireProgress() {
    this.connectionProgress = undefined;
    this.activationProgress = undefined;
    this.createIntent.reset();
    this.rotateProviderCredentialIntent.reset();
    this.activateProviderIntent.reset();
  }
  private capturePrincipal = usePrincipalLifetime(() => {
    this.ownerEpoch += 1;
    this.retireProgress();
    if (this.draft) {
      this.draft.credential = '';
      this.draft.externalCredentialReference = '';
    }
    this.grantInput = '';
    this.busy = this.errorMessage = this.notice = '';
    this.validationIssues = [];
  });
  private captureOwner = () => {
    const principalCurrent = this.capturePrincipal();
    const epoch = this.ownerEpoch;
    return () => principalCurrent() && epoch === this.ownerEpoch;
  };
  run = async (
    label: string,
    action: (isCurrent: () => boolean) => Promise<void>
  ): Promise<boolean> => {
    if (this.busy || !this.canManage) return false;
    const isCurrent = this.captureOwner();
    this.busy = label;
    this.errorMessage = '';
    this.validationIssues = [];
    this.notice = '';
    try {
      await action(isCurrent);
      return isCurrent();
    } catch (error) {
      if (!isCurrent()) return false;
      const progress = this.connectionProgress;
      if (progress?.phase === 'create' || progress?.phase === 'update') {
        if (this.refused(error) && !progress.uncertain) {
          this.connectionProgress = undefined;
          this.createIntent.reset();
        } else if (!this.refused(error))
          this.connectionProgress = { ...progress, uncertain: true };
      }
      if (this.wizardProvider && isEtagMismatch(error))
        this.wizardConflict = true;
      else {
        this.errorMessage = errorMessage(error);
        this.validationIssues = fieldIssues(error);
      }
      return false;
    } finally {
      if (isCurrent()) this.busy = '';
    }
  };
  clearCertificationResults = () => {
    this.activationProgress = undefined;
    this.activateProviderIntent.reset();
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
    await this.run('reload', async (isCurrent) => {
      await this.refetchWizardModels();
      if (!isCurrent()) return;
      this.wizardModelReloadVersion += 1;
      this.wizardConflict = false;
    });
  };
  goBack = () => {
    if (this.busy || this.wizardStep <= 1) return;
    if (this.activateProviderIntent.unresolved) {
      this.errorMessage =
        'Activation may already have committed. Retry activation before changing this connection, or leave the wizard.';
      return;
    }
    this.wizardStep -= 1;
    const provider = this.wizardProvider;
    const spec = this.selectedSpec;
    const previous = this.draft;
    if (this.wizardStep !== 1 || !provider || !spec || !previous) return;
    // Discovery records upstream model facts in the stored configuration;
    // re-saving the pre-discovery document would erase them and reset
    // certification evidence, so the connection form edits the stored one.
    const draft = createProviderDraft(spec, provider.configuration);
    draft.name = provider.name;
    draft.projectId = previous.projectId;
    draft.model = previous.model;
    draft.credential = '';
    this.draft = draft;
  };

  startAnother = () => {
    this.ownerEpoch += 1;
    this.retireProgress();
    if (this.draft) {
      this.draft.credential = '';
      this.draft.externalCredentialReference = '';
    }
    this.providerId = '';
    this.draft = null;
    this.wizardStep = 1;
    this.probe = null;
    this.grant.abandon();
    this.grantInput = '';
    this.manualModelNames = '';
    this.busy = '';
    this.errorMessage = '';
    this.validationIssues = [];
    this.notice = '';
    this.certificationResults = {};
    this.wizardConflict = false;
    resetCursor(this.wizardModelPagination);
  };
  createDraft = async (event: SubmitEvent) => {
    event.preventDefault();
    if (!this.canManage || this.busy || !this.draft || !this.selectedSpec)
      return;
    const current = this.draft;
    const spec = this.selectedSpec;

    const existing = this.wizardProvider;
    const issue = validateProviderDraft(current, spec, {
      credentialAlreadyStored: Boolean(
        existing || this.connectionProgress?.phase === 'saved'
      ),

      profiles: this.queryClient.getQueryData<ProviderProfile[]>(
        providerKeys.profiles()
      ),
      vendors: this.queryClient.getQueryData<ProviderVendor[]>(
        providerKeys.vendors()
      )
    });
    if (issue) {
      this.errorMessage = issue;
      this.validationIssues = [];
      return;
    }
    const input = buildCreateProviderInput(current, spec);
    const previous = this.connectionProgress;
    const changed =
      previous &&
      stringifyNativeJSON(previous.input) !== stringifyNativeJSON(input);
    const credentialCorrection =
      previous?.phase === 'credential' &&
      previous.status === 'refused' &&
      stringifyNativeJSON({
        ...previous.input,
        credential: undefined,
        credential_reference: undefined
      }) ===
        stringifyNativeJSON({
          ...input,
          credential: undefined,
          credential_reference: undefined
        });
    if (changed && !credentialCorrection) {
      this.errorMessage =
        previous.phase === 'saved'
          ? 'Connection saved. Retry the original fields to refresh and test it before editing, or leave this wizard.'
          : 'The previous submission may already have saved. Retry its original fields before editing, or leave this wizard.';
      return;
    }
    const progress: ConnectionProgress = credentialCorrection
      ? { ...previous, input: snapshotNativeJSON(input), status: 'ready' }
      : (previous ??
        (existing
          ? {
              phase: 'update',
              input: snapshotNativeJSON(input),
              provider: { id: existing.id, etag: existing.etag },
              uncertain: false
            }
          : {
              phase: 'create',
              input: snapshotNativeJSON(input),
              uncertain: false
            }));
    this.connectionProgress = progress;
    if (!previous) {
      this.activationProgress = undefined;
      this.activateProviderIntent.reset();
    }
    await this.run('create', async (isCurrent) => {
      const submission = await this.saveConnection(progress, isCurrent);
      if (!submission || !isCurrent()) return;
      current.credential = '';
      this.providerId = submission.id;
      this.notice =
        'Provider saved. Connection testing remains a separate step.';
      const refreshed = await refreshProviderReceipt(
        this.queryClient,
        {
          change: { kind: 'connection' },
          notice: this.notice
        },
        this.refetchWizardModels,
        isCurrent
      );
      if (refreshed.kind === 'stale') return;
      if (refreshed.kind === 'refresh-failed') {
        this.errorMessage = `The provider change succeeded, but refreshing failed. ${refreshed.message}`;
        return;
      }
      const snapshot = refreshed.value;
      if (this.grantRequired) {
        const enrolled = await this.holdsLiveGrant(snapshot.provider);
        if (!isCurrent()) return;
        if (!enrolled) {
          await this.grant.start(snapshot.provider);
          if (isCurrent() && this.grantEnrollment)
            this.connectionProgress = undefined;
          return;
        }
      }
      await this.testConnection(snapshot.provider, isCurrent);
      if (isCurrent()) this.connectionProgress = undefined;
    });
  };
  private saveConnection = async (
    progress: ConnectionProgress,
    isCurrent: () => boolean
  ): Promise<Extract<ConnectionProgress, { phase: 'saved' }> | undefined> => {
    if (progress.phase === 'saved') return progress;
    if (progress.phase === 'update') {
      const updated = await updateProvider(
        progress.provider.id,
        progress.provider.etag,
        {
          name: progress.input.name,
          configuration: progress.input.configuration
        }
      );
      if (!isCurrent()) return;
      const next: ConnectionProgress = {
        phase: 'credential',
        input: progress.input,
        provider: { id: updated.id, etag: updated.etag },
        status: 'ready'
      };
      this.connectionProgress = next;
      this.notice = 'Connection configuration saved.';
      return this.saveConnection(next, isCurrent);
    }
    if (progress.phase === 'credential') {
      const credential =
        progress.input.credential_reference ?? progress.input.credential;
      if (credential) {
        try {
          await this.rotateProviderCredentialIntent.submit(
            [progress.provider, credential] as const,
            (input, requestId) => rotateProviderCredential(...input, requestId)
          );
        } catch (error) {
          if (isCurrent()) {
            const refused =
              this.refused(error) && progress.status !== 'uncertain';
            this.connectionProgress = {
              ...progress,
              status: refused ? 'refused' : 'uncertain'
            };
            if (refused) this.rotateProviderCredentialIntent.reset();
          }
          throw error;
        }
      }
      if (!isCurrent()) return;
      return this.connectionSaved(progress.input, progress.provider.id);
    }
    const request = this.createIntent.prepare(progress.input);
    const id = await createProvider(request.input, request.requestId);
    if (!isCurrent()) return;
    this.createIntent.reset();
    return this.connectionSaved(progress.input, id);
  };
  private connectionSaved(
    input: ConnectionInput,
    id: string
  ): Extract<ConnectionProgress, { phase: 'saved' }> {
    const progress = {
      phase: 'saved' as const,
      id,
      input: snapshotNativeJSON({
        ...input,
        credential: input.credential === undefined ? undefined : ''
      })
    };
    this.connectionProgress = progress;
    return progress;
  }

  private holdsLiveGrant = async (provider: Provider) => {
    if (!provider.draft_credential_id) return false;
    const credentials = await this.queryClient.fetchQuery({
      queryKey: providerKeys.credentials(provider.id),
      queryFn: ({ signal }) => listProviderCredentials(provider.id, signal),
      staleTime: 0
    });
    return isLiveGrant(
      credentials,
      provider.draft_credential_id,
      provider.configuration.profile_revision
    );
  };

  private testEnrolledGrant = async (isCurrent: () => boolean) => {
    await this.queryClient.invalidateQueries(
      {
        queryKey: providerKeys.credentials(this.providerId)
      },
      { throwOnError: true }
    );
    if (!isCurrent()) return;
    const snapshot = await this.refetchWizardModels();
    if (isCurrent()) await this.testConnection(snapshot.provider, isCurrent);
  };
  private testConnection = async (
    provider: Provider,
    isCurrent: () => boolean
  ) => {
    const { result: probe } = await probeProvider(provider);
    if (!isCurrent()) return;
    this.probe = probe;
    if (!this.probe.succeeded) throw new Error(this.probe.detail);
    this.wizardStep = 2;
  };
  continueGrantEnrollment = () => this.grant.continue();
  pollGrantEnrollment = () => this.grant.poll();
  cancelGrantEnrollment = () => this.grant.cancel();
  discoverWizardProvider = async () => {
    const provider = this.wizardProvider;
    if (!provider) return;
    await this.run('discover', async (isCurrent) => {
      const discovered = await discoverProviderModels(provider);
      if (!isCurrent()) return;
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
      if (!isCurrent()) return;
      this.wizardStep = 2;
      await this.queryClient.invalidateQueries({
        queryKey: providerKeys.modelCatalog
      });
    });
  };
  declareWizardModels = async () => {
    const provider = this.wizardProvider;
    if (!provider) return;
    const names = parseManualModelNames(this.manualModelNames);
    if (!names.length) {
      this.errorMessage = 'Enter at least one upstream model identifier.';
      return;
    }
    await this.run('declare-models', async (isCurrent) => {
      await declareProviderModels(provider, names);
      if (!isCurrent()) return;
      this.clearCertificationResults();
      this.manualModelNames = '';
      resetCursor(this.wizardModelPagination);
      await this.refetchWizardModels();
      if (!isCurrent()) return;
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
    const provider = this.wizardProvider;
    if (!provider) return false;
    return this.run(`model-${modelId}`, async (isCurrent) => {
      await setProviderModel(
        { ...provider, etag: providerEtag },
        modelId,
        enabled,
        capabilities
      );
      if (!isCurrent()) return;
      this.clearCertificationResults();
      await this.refetchWizardModels();
      if (!isCurrent()) return;
      await this.queryClient.invalidateQueries({
        queryKey: providerKeys.modelCatalog
      });
      if (!isCurrent()) return;
      await this.queryClient.invalidateQueries({ queryKey: overviewKeys.root });
      if (!isCurrent()) return;
      this.notice = 'Capability review saved with declared provenance.';
    });
  };
  certifyWizardModel = async (modelId: string) => {
    const current = this.wizardProvider;
    if (!current) return;
    await this.run(`certify-${modelId}`, async (isCurrent) => {
      let provider = current;
      if (!certificationPrerequisiteReady(provider)) {
        const { result: probe } = await probeProvider(provider);
        if (!isCurrent()) return;
        this.probe = probe;
        if (!this.probe.succeeded) throw new Error(this.probe.detail);

        provider = (await this.refetchWizardModels()).provider;
        if (!isCurrent()) return;
      }
      const { result } = await certifyProviderModel(provider, modelId);
      if (!isCurrent()) return;
      this.certificationResults = {
        ...this.certificationResults,
        [modelId]: result
      };
      await this.refetchWizardModels();
      if (!isCurrent()) return;
      await this.queryClient.invalidateQueries({
        queryKey: providerKeys.modelCatalog
      });
      if (!isCurrent()) return;
      this.probe = null;
      this.notice = `${result.certified_count} of ${result.attempted_count} reviewed tuples passed server certification. Test the completed draft before activation.`;
    });
  };
  testWizardDraftForActivation = async () => {
    const provider = this.wizardProvider;
    if (!provider) return;
    await this.run('final-probe', async (isCurrent) => {
      const { result: probe } = await probeProvider(provider);
      if (!isCurrent()) return;
      this.probe = probe;
      if (!this.probe.succeeded) throw new Error(this.probe.detail);
      await this.refetchWizardModels();
      if (!isCurrent()) return;
      this.notice = `Final draft test passed: ${probeSummary(this.probe)}`;
    });
  };
  activateWizardProvider = async () => {
    const provider = this.wizardProvider;
    if (!provider) return;
    const progress: ActivationProgress = this.activationProgress ?? {
      input: { id: provider.id, etag: provider.etag },
      phase: 'pending',
      uncertain: false
    };
    this.activationProgress = progress;
    await this.run('activate', async (isCurrent) => {
      const activate = async (): Promise<
        Extract<ActivationProgress, { phase: 'committed' }> | undefined
      > => {
        if (progress.phase === 'committed') return progress;
        try {
          const generation = await this.activateProviderIntent.submit(
            progress.input,
            (input, requestId) => activateProvider(input, requestId)
          );
          if (!isCurrent()) return;
          const committed = {
            phase: 'committed' as const,
            input: progress.input,
            generation
          };
          this.activationProgress = committed;
          return committed;
        } catch (error) {
          if (isCurrent()) {
            if (this.refused(error) && !progress.uncertain) {
              this.activationProgress = undefined;
              this.activateProviderIntent.reset();
            } else if (!this.refused(error))
              this.activationProgress = { ...progress, uncertain: true };
          }
          throw error;
        }
      };
      const committed = await activate();
      if (!committed || !isCurrent()) return;
      this.notice = `Provider activated in runtime generation ${committed.generation}.`;
      const refreshed = await refreshProviderReceipt(
        this.queryClient,
        {
          change: { kind: 'activation', id: provider.id },
          notice: this.notice
        },
        async () => {
          const snapshot = await this.refetchWizardModels();
          if (isCurrent()) this.wizardStep = 3;
          return snapshot;
        },
        isCurrent
      );
      if (refreshed.kind === 'stale') return;
      if (refreshed.kind === 'refresh-failed') {
        this.errorMessage = `The provider change succeeded, but refreshing failed. ${refreshed.message}`;
        return;
      }
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
    this.draftCredentials = createQuery(() => ({
      queryKey: providerKeys.credentials(this.providerId),
      queryFn: ({ signal }) => listProviderCredentials(this.providerId, signal),
      enabled:
        this.grantRequired && Boolean(this.wizardProvider?.draft_credential_id)
    }));
    this.capabilityOptions = createQuery(() => {
      const kind = this.wizardProvider?.configuration.kind;
      return {
        queryKey: providerKeys.capabilityOptions(kind ?? ''),
        queryFn: kind
          ? ({ signal }: { signal: AbortSignal }) =>
              getProviderCapabilityOptions(kind, signal)
          : skipToken
      };
    });
    $effect(() => {
      const first = this.providerKinds.data?.[0];
      if (!this.draft && first) this.draft = createProviderDraft(first);
      const current = this.draft;
      const spec = this.selectedSpec;
      if (!current || !spec) return;
      if (!this.credentialRequired) current.credential = '';
    });
    $effect(() => {
      const copy = globalThis.location
        ? new SvelteURLSearchParams(globalThis.location.search).get('copy')
        : null;
      if (!copy || this.copyLoaded || !this.draft || !this.selectedSpec) return;
      this.copyLoaded = true;
      const isCurrent = this.captureOwner();
      const initialDraft = this.draft;
      const initialInput =
        initialDraft && this.selectedSpec
          ? stringifyNativeJSON(
              buildCreateProviderInput(initialDraft, this.selectedSpec)
            )
          : null;
      const canApplyCopy = () => {
        if (
          !isCurrent() ||
          this.providerId ||
          this.connectionProgress ||
          this.draft !== initialDraft
        )
          return false;
        try {
          return (
            initialInput ===
            (this.draft && this.selectedSpec
              ? stringifyNativeJSON(
                  buildCreateProviderInput(this.draft, this.selectedSpec)
                )
              : null)
          );
        } catch {
          return false;
        }
      };
      void getProvider(copy)
        .then((source) => {
          if (!canApplyCopy()) return;
          const spec = this.providerKinds.data?.find(
            (kind) => kind.kind === source.configuration.kind
          );
          if (!spec) return;
          const duplicate = createProviderDraft(spec, source.configuration);
          duplicate.name = `${source.name} copy`;
          duplicate.credential = '';
          // Network credentials are provider-owned and cannot be reused by a copy.
          if (duplicate.document?.at(networkCredentialPath) !== undefined) {
            duplicate.document.set(networkCredentialPath, undefined);
            this.notice =
              'The copied connection needs its own network credential. The source credential reference was removed.';
          }
          this.draft = duplicate;
        })
        .catch((error) => {
          if (!canApplyCopy()) return;
          this.errorMessage = errorMessage(error);
        });
    });
    onDestroy(() => {
      void this.queryClient.cancelQueries({
        queryKey: providerKeys.modelsOf(this.providerId)
      });
    });
  }
}
