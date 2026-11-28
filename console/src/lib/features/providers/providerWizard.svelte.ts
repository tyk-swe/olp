import { overviewKeys } from '#lib/features/overview/index.js';
import { stringifyNativeJSON } from '#lib/nativeJson.js';
import { networkCredentialPath } from './configurationDraft.svelte';
import { refreshProviderReceipt } from './providerChanges';
import { providerKeys } from '#lib/domain/providers/providerKeys.js';
import { createQuery, skipToken, useQueryClient } from '@tanstack/svelte-query';
import { onDestroy } from 'svelte';
import { SvelteURLSearchParams } from 'svelte/reactivity';
import {
  errorMessage,
  fieldIssues,
  isEtagMismatch,
  type FieldIssue
} from '#lib/management/http.js';
import { emptyCursorHistory, resetCursor } from '#lib/listing/pagination.js';
import {
  probeProvider,
  type ProviderProbe
} from '#lib/features/providers/providers.js';
import {
  getProvider,
  type Provider,
  type ProviderVendor
} from '#lib/domain/providers/api.js';
import {
  certifyProviderModel,
  declareProviderModels,
  discoverProviderModels,
  getProviderCapabilityOptions,
  listProviderModelPage,
  setProviderModel,
  type CapabilityDeclaration,
  type CapabilityCertification
} from '#lib/features/providers/models.js';
import type { ProviderProfile } from '#lib/features/providers/providerProfiles.js';
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
import { useRole } from '#lib/session/useRole.svelte.js';
import {
  EditorSession,
  useMutationIntentGroup
} from '#lib/editing/editorSession.svelte.js';
import {
  commitProviderActivation,
  isLiveGrant,
  isRefusedProblem,
  resolveConnectionProgress,
  saveProviderConnection,
  type ActivationProgress,
  type ConnectionProgress
} from './providerWizardConnection';
import { providerQueries } from '#lib/domain/providers/queries.js';

export class ProviderWizardState {
  private connectionProgress?: ConnectionProgress;
  private activationProgress?: ActivationProgress;
  private readonly intents = useMutationIntentGroup([
    'create',
    'rotateCredential',
    'activate'
  ]);
  readonly createIntent = this.intents.create;
  readonly rotateProviderCredentialIntent = this.intents.rotateCredential;
  readonly activateProviderIntent = this.intents.activate;

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

  private retireProgress() {
    this.connectionProgress = undefined;
    this.activationProgress = undefined;
    this.intents.resetAll();
  }

  private readonly session = new EditorSession(() => {
    this.retireProgress();
    if (this.draft) {
      this.draft.credential = '';
      this.draft.externalCredentialReference = '';
    }
    this.grantInput = '';
    this.busy = this.errorMessage = this.notice = '';
    this.validationIssues = [];
  });

  run = async (
    label: string,
    action: (isCurrent: () => boolean) => Promise<void>
  ): Promise<boolean> => {
    if (this.busy || !this.canManage) return false;
    const { isCurrent } = this.session.capture();
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
        if (isRefusedProblem(error) && !progress.uncertain) {
          this.connectionProgress = undefined;
          this.intents.create.reset();
        } else if (!isRefusedProblem(error)) {
          this.connectionProgress = { ...progress, uncertain: true };
        }
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
    this.intents.activate.reset();
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
    if (this.intents.activate.unresolved) {
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
    this.session.advance();
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
    const resolved = resolveConnectionProgress(
      buildCreateProviderInput(current, spec),
      this.connectionProgress,
      existing
    );
    if (resolved.error || !resolved.progress) {
      this.errorMessage = resolved.error ?? '';
      return;
    }
    const hadPrevious = Boolean(this.connectionProgress);
    const progress = resolved.progress;
    this.connectionProgress = progress;
    if (!hadPrevious) {
      this.activationProgress = undefined;
      this.intents.activate.reset();
    }
    await this.run('create', async (isCurrent) => {
      const submission = await saveProviderConnection(progress, {
        createIntent: this.intents.create,
        rotateIntent: this.intents.rotateCredential,
        isCurrent,
        setProgress: (next) => {
          this.connectionProgress = next;
        },
        setNotice: (notice) => {
          this.notice = notice;
        }
      });
      if (!submission || !isCurrent()) return;
      current.credential = '';
      this.providerId = submission.id;
      this.notice =
        'Provider saved. Connection testing remains a separate step.';
      const refreshed = await refreshProviderReceipt(
        this.queryClient,
        { change: { kind: 'connection' }, notice: this.notice },
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

  private holdsLiveGrant = async (provider: Provider) => {
    if (!provider.draft_credential_id) return false;
    const credentials = await this.queryClient.fetchQuery({
      ...providerQueries.credentials(provider.id),
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
      { queryKey: providerKeys.credentials(this.providerId) },
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
      const committed = await commitProviderActivation(progress, {
        activateIntent: this.intents.activate,
        isCurrent,
        setProgress: (next) => {
          this.activationProgress = next;
        }
      });
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
      }
    });
  };

  private loadCopiedProvider() {
    const copy = globalThis.location
      ? new SvelteURLSearchParams(globalThis.location.search).get('copy')
      : null;
    if (!copy || this.copyLoaded || !this.draft || !this.selectedSpec) return;
    this.copyLoaded = true;
    const { isCurrent } = this.session.capture();
    const initialDraft = this.draft;
    const initialInput = stringifyNativeJSON(
      buildCreateProviderInput(initialDraft, this.selectedSpec)
    );
    const canApplyCopy = () => {
      if (
        !isCurrent() ||
        this.providerId ||
        this.connectionProgress ||
        this.draft !== initialDraft ||
        !this.selectedSpec
      )
        return false;
      try {
        return (
          initialInput ===
          stringifyNativeJSON(
            buildCreateProviderInput(this.draft, this.selectedSpec)
          )
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
  }

  constructor() {
    this.providerKinds = createQuery(providerQueries.kinds);
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
      ...providerQueries.credentials(this.providerId),
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
      this.loadCopiedProvider();
    });
    onDestroy(() => {
      void this.queryClient.cancelQueries({
        queryKey: providerKeys.modelsOf(this.providerId)
      });
    });
  }
}
