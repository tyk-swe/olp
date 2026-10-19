import { snapshotNativeJSON, stringifyNativeJSON } from '$lib/json/nativeJson';
import { overviewKeys } from '$lib/features/overview/overviewKeys';
import { providerKeys } from '$lib/features/providers/providerKeys';
import { createQuery, skipToken, useQueryClient } from '@tanstack/svelte-query';
import { onDestroy } from 'svelte';
import { SvelteURLSearchParams } from 'svelte/reactivity';
import {
  ApiProblem,
  errorMessage,
  fieldIssues,
  isEtagMismatch,
  type FieldIssue
} from '$lib/api/http';
import { emptyCursorHistory, resetCursor } from '$lib/lists/pagination';
import {
  getProvider,
  activateProvider,
  createProvider,
  probeProvider,
  updateProvider,
  type Provider,
  type ProviderProbe
} from '$lib/features/providers/api/providers';
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
} from '$lib/features/providers/api/models';
import {
  listProviderCredentials,
  rotateProviderCredential,
  type ProviderCredential
} from '$lib/features/providers/api/credentials';
import type { ProviderProfile } from '$lib/features/providers/api/profiles';
import type { ProviderVendor } from '$lib/features/providers/api/providers';
import type { GrantEnrollment } from './api/grants';
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
} from '$lib/features/providers/providerEditor';
import { useRole } from '$lib/features/access/session/useRole.svelte';
import {
  useMutationIntent,
  usePrincipalLifetime
} from '$lib/forms/mutationIntent.svelte';

/** Reports whether the credential version with the id is a live grant
 * enrolled through the plugin build with the digest, which a provider pinning
 * that build authenticates with: its grant hasn't lapsed and the version isn't
 * revoked. */
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

type ConnectionProgress = {
  input: Parameters<typeof createProvider>[0];
  existing?: Pick<Provider, 'id' | 'etag'>;
  configured?: Pick<Provider, 'id' | 'etag'>;
  id?: string;
  saved: boolean;
  credentialRefused?: boolean;
  writeAmbiguous?: boolean;
  credentialAmbiguous?: boolean;
};

type ActivationProgress = {
  input: Pick<Provider, 'id' | 'etag'>;
  generation?: number;
  ambiguous?: boolean;
};

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
  /** The draft provider's credential versions, while its profile takes a
   * grant. */
  draftCredentials;
  probe = $state<ProviderProbe | null>(null);
  /** The grant enrollment the operator is signing in through, if any. */
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
  /** What the operator pastes back from the upstream's sign-in. */
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
  /** Set while a live grant, enrolled through the plugin build the
   * Connection stage pins, backs the draft, so saving tests the connection
   * rather than signing in upstream. */
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
    if (this.connectionProgress) {
      this.connectionProgress.input = snapshotNativeJSON({
        ...this.connectionProgress.input,
        credential: undefined,
        credential_reference: undefined
      });
    }
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
      if (progress && !progress.id && !progress.configured) {
        if (this.refused(error) && !progress.writeAmbiguous) {
          this.connectionProgress = undefined;
          this.createIntent.reset();
        } else if (!this.refused(error)) progress.writeAmbiguous = true;
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
  /** Return to a blank step 1 without leaving the route; the draft effect
   * recreates the connection form from the first provider kind. */
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
    // The connector kind is locked once the draft exists, so stepping back
    // always edits that provider rather than orphaning it behind a second one.
    const existing = this.wizardProvider;
    const issue = validateProviderDraft(current, spec, {
      // The first pass stored a write-only credential; the field is cleared
      // afterwards and must not be demanded again on a re-save.
      credentialAlreadyStored: Boolean(
        existing || this.connectionProgress?.saved
      ),
      // The connection form loads the profile catalogue.
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
    let progress = this.connectionProgress;
    if (
      progress &&
      stringifyNativeJSON(progress.input) !== stringifyNativeJSON(input)
    ) {
      if (
        progress.credentialRefused &&
        stringifyNativeJSON({
          ...progress.input,
          credential: undefined,
          credential_reference: undefined
        }) ===
          stringifyNativeJSON({
            ...input,
            credential: undefined,
            credential_reference: undefined
          })
      ) {
        progress.input = snapshotNativeJSON(input);
        progress.credentialRefused = false;
      } else {
        this.errorMessage = progress.saved
          ? 'Connection saved. Retry the original fields to refresh and test it before editing, or leave this wizard.'
          : 'The previous submission may already have saved. Retry its original fields before editing, or leave this wizard.';
        return;
      }
    }
    if (!progress) {
      progress = {
        input: snapshotNativeJSON(input),
        existing: existing
          ? { id: existing.id, etag: existing.etag }
          : undefined,
        saved: false
      };
      this.connectionProgress = progress;
      this.activationProgress = undefined;
      this.activateProviderIntent.reset();
    }
    const submission = progress;
    await this.run('create', async (isCurrent) => {
      if (!submission.id) {
        if (submission.existing) {
          if (!submission.configured) {
            const updated = await updateProvider(
              submission.existing.id,
              submission.existing.etag,
              {
                name: submission.input.name,
                configuration: submission.input.configuration
              }
            );
            if (!isCurrent()) return;
            submission.configured = { id: updated.id, etag: updated.etag };
            this.notice = 'Connection configuration saved.';
          }
          const credential =
            submission.input.credential_reference ??
            submission.input.credential;
          if (credential) {
            submission.credentialRefused = false;
            try {
              await this.rotateProviderCredentialIntent.submit(
                [submission.configured, credential] as const,
                (input, requestId) =>
                  rotateProviderCredential(...input, requestId)
              );
            } catch (error) {
              if (isCurrent()) {
                if (this.refused(error) && !submission.credentialAmbiguous) {
                  submission.credentialRefused = true;
                  this.rotateProviderCredentialIntent.reset();
                } else if (!this.refused(error))
                  submission.credentialAmbiguous = true;
              }
              throw error;
            }
            if (!isCurrent()) return;
          }
          submission.id = submission.configured.id;
        } else {
          const request = this.createIntent.prepare(submission.input);
          const id = await createProvider(request.input, request.requestId);
          if (!isCurrent()) return;
          submission.id = id;
          this.createIntent.reset();
        }
        submission.saved = true;
        submission.input = snapshotNativeJSON({
          ...submission.input,
          credential:
            requiresCredential(spec, current.authMode) &&
            !current.useExternalCredential
              ? ''
              : undefined
        });
        current.credential = '';
      }
      this.providerId = submission.id;
      this.notice =
        'Provider saved. Connection testing remains a separate step.';
      const snapshot = await this.refetchWizardModels();
      if (!isCurrent()) return;
      await Promise.all([
        this.queryClient.invalidateQueries(
          { queryKey: providerKeys.summaries },
          { throwOnError: true }
        ),
        this.queryClient.invalidateQueries(
          { queryKey: providerKeys.modelCatalog },
          { throwOnError: true }
        )
      ]);
      if (!isCurrent()) return;
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
  /** Reports whether a live grant, enrolled through the plugin build the
   * provider pins, backs its draft now. */
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
  /** Tests the connection with the grant a completed enrollment staged. */
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
        // Refresh the observation before certifying the reviewed model.
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
      input: { id: provider.id, etag: provider.etag }
    };
    this.activationProgress = progress;
    await this.run('activate', async (isCurrent) => {
      if (progress.generation === undefined) {
        let generation: number;
        try {
          generation = await this.activateProviderIntent.submit(
            progress.input,
            (input, requestId) => activateProvider(input, requestId)
          );
        } catch (error) {
          if (isCurrent()) {
            if (this.refused(error) && !progress.ambiguous) {
              this.activationProgress = undefined;
              this.activateProviderIntent.reset();
            } else if (!this.refused(error)) progress.ambiguous = true;
          }
          throw error;
        }
        if (!isCurrent()) return;
        progress.generation = generation;
      }
      this.notice = `Provider activated in runtime generation ${progress.generation}.`;
      await this.refetchWizardModels();
      if (!isCurrent()) return;
      this.wizardStep = 3;
      await Promise.all([
        this.queryClient.invalidateQueries(
          { queryKey: providerKeys.summaries },
          { throwOnError: true }
        ),
        this.queryClient.invalidateQueries(
          { queryKey: providerKeys.modelCatalog },
          { throwOnError: true }
        ),
        this.queryClient.invalidateQueries(
          { queryKey: overviewKeys.root },
          { throwOnError: true }
        )
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
          // Editing an incomplete draft also retires the original copy result.
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
          if (
            duplicate.document?.at(['options', 'network', 'credential_id']) !==
            undefined
          ) {
            duplicate.document.set(
              ['options', 'network', 'credential_id'],
              undefined
            );
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
