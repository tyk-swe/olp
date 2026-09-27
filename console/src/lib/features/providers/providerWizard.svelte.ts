import { overviewKeys } from '$lib/features/overview/overviewKeys';
import { providerKeys } from '$lib/features/providers/providerKeys';
import { createQuery, skipToken, useQueryClient } from '@tanstack/svelte-query';
import { onDestroy } from 'svelte';
import { SvelteURLSearchParams } from 'svelte/reactivity';
import {
  errorMessage as message,
  fieldIssues,
  isEtagMismatch,
  unanswered,
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
import type { ProviderProfile } from '$lib/features/providers/profiles';
import {
  cancelGrantEnrollment,
  continueGrantEnrollment,
  pollGrantEnrollment,
  startGrantEnrollment,
  type GrantEnrollment,
  type GrantEnrollmentStatus
} from '$lib/features/providers/grants';
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

export class ProviderWizardState {
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
  probe = $state<ProviderProbe | null>(null);
  /** The grant enrollment the operator is signing in through, if any. */
  grantEnrollment = $state<GrantEnrollment | null>(null);
  /** What the operator pastes back from the upstream's sign-in. */
  grantInput = $state('');
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
  grantRequired = $derived(
    Boolean(
      this.draft &&
      this.selectedSpec &&
      requiresGrant(this.selectedSpec, this.draft.authMode)
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
  /** Return to a blank step 1 without leaving the route; the draft effect
   * recreates the connection form from the first provider kind. */
  startAnother = () => {
    this.providerId = '';
    this.draft = null;
    this.wizardStep = 1;
    this.probe = null;
    this.grantEnrollment = null;
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
    if (!this.draft || !this.selectedSpec) return;
    const current = this.draft;
    const spec = this.selectedSpec;
    // The connector kind is locked once the draft exists, so stepping back
    // always edits that provider rather than orphaning it behind a second one.
    const existing = this.wizardProvider;
    const issue = validateProviderDraft(current, spec, {
      // The first pass stored a write-only credential; the field is cleared
      // afterwards and must not be demanded again on a re-save.
      credentialAlreadyStored: Boolean(existing),
      // The connection form loads the profile catalogue.
      profiles: this.queryClient.getQueryData<ProviderProfile[]>([
        'provider-profiles'
      ])
    });
    if (issue) {
      this.errorMessage = issue;
      this.validationIssues = [];
      return;
    }
    await this.run('create', async () => {
      const input = buildCreateProviderInput(current, spec);
      let id: string;
      if (existing) {
        const updated = await updateProvider(existing.id, existing.etag, {
          name: input.name,
          configuration: input.configuration
        });
        if (current.credential) {
          await rotateProviderCredential(updated, current.credential);
        }
        id = updated.id;
      } else {
        id = await createProvider(input);
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
      // A grant comes from the operator's sign-in upstream, which the grant
      // enrollment panel collects before the connection is tested.
      if (this.grantRequired && !snapshot.provider.draft_credential_id) {
        this.grantEnrollment = await startGrantEnrollment(snapshot.provider);
        return;
      }
      await this.testConnection(snapshot.provider);
    });
  };
  private testConnection = async (provider: Provider) => {
    this.probe = await probeProvider(provider);
    if (!this.probe.succeeded) throw new Error(this.probe.detail);
    this.wizardStep = 2;
  };
  /** Exchanges what the operator pasted back for a grant, which becomes the
   * draft's credential version, then tests the connection with it. */
  continueGrantEnrollment = async () => {
    const enrollment = this.grantEnrollment;
    const input = this.grantInput.trim();
    if (!enrollment) return;
    if (!input) {
      this.errorMessage =
        'Paste the callback URL, or the code the upstream displayed.';
      this.validationIssues = [];
      return;
    }
    await this.run('grant', async () => {
      try {
        await continueGrantEnrollment(enrollment, input);
      } finally {
        // A grant enrollment is continued once, whether or not it succeeds.
        this.grantEnrollment = null;
        this.grantInput = '';
      }
      await this.testConnection((await this.refetchWizardModels()).provider);
    });
  };
  /** Asks whether the operator approved the device upstream, and returns how
   * many seconds to wait before asking again, or null once the enrollment
   * ended. On approval, the grant is the draft's credential version, and the
   * connection is tested with it. A request that fails without an answer,
   * such as while OLP restarts, is asked again. */
  pollGrantEnrollment = async (): Promise<number | null> => {
    const enrollment = this.grantEnrollment;
    if (!enrollment) return null;
    let status: GrantEnrollmentStatus | undefined;
    let failure: unknown;
    try {
      status = await pollGrantEnrollment(enrollment);
    } catch (error) {
      failure = error;
    }
    // Cancelled meanwhile.
    if (this.grantEnrollment !== enrollment) return null;
    const interval = enrollment.device?.interval ?? null;
    if (status?.status === 'pending') return status.interval ?? interval;
    if (unanswered(failure)) return interval;
    this.grantEnrollment = null;
    if (status?.status === 'completed') {
      await this.run('grant', async () =>
        this.testConnection((await this.refetchWizardModels()).provider)
      );
      return null;
    }
    this.errorMessage =
      status?.status === 'denied'
        ? 'The device sign-in was denied upstream. Save and sign in upstream to try again.'
        : status?.status === 'expired'
          ? 'The device sign-in expired before it was approved. Save and sign in upstream to try again.'
          : message(failure);
    this.validationIssues = [];
    return null;
  };
  cancelGrantEnrollment = async () => {
    const enrollment = this.grantEnrollment;
    if (!enrollment) return;
    await this.run('grant-cancel', async () => {
      // Abandoned at once, so a status request answering meanwhile is ignored.
      this.grantEnrollment = null;
      this.grantInput = '';
      await cancelGrantEnrollment(enrollment);
    });
  };
  discoverWizardProvider = async () => {
    const provider = this.wizardProvider;
    if (!provider) return;
    await this.run('discover', async () => {
      const discovered = await discoverProviderModels(provider);
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
    const provider = this.wizardProvider;
    if (!provider) return;
    const names = parseManualModelNames(this.manualModelNames);
    if (!names.length) {
      this.errorMessage = 'Enter at least one upstream model identifier.';
      return;
    }
    await this.run('declare-models', async () => {
      await declareProviderModels(provider, names);
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
    const provider = this.wizardProvider;
    if (!provider) return false;
    return this.run(`model-${modelId}`, async () => {
      await setProviderModel(
        { ...provider, etag: providerEtag },
        modelId,
        enabled,
        capabilities
      );
      this.clearCertificationResults();
      await this.refetchWizardModels();
      await this.queryClient.invalidateQueries({
        queryKey: providerKeys.modelCatalog
      });
      await this.queryClient.invalidateQueries({ queryKey: overviewKeys.root });
      this.notice = 'Capability review saved with declared provenance.';
    });
  };
  certifyWizardModel = async (modelId: string) => {
    const current = this.wizardProvider;
    if (!current) return;
    await this.run(`certify-${modelId}`, async () => {
      let provider = current;
      if (!certificationPrerequisiteReady(provider)) {
        this.probe = await probeProvider(provider);
        if (!this.probe.succeeded) throw new Error(this.probe.detail);
        // The probe advances the provider ETag, so certify the refreshed record.
        provider = (await this.refetchWizardModels()).provider;
      }
      const result = await certifyProviderModel(provider, modelId);
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
    const provider = this.wizardProvider;
    if (!provider) return;
    await this.run('final-probe', async () => {
      this.probe = await probeProvider(provider);
      if (!this.probe.succeeded) throw new Error(this.probe.detail);
      await this.refetchWizardModels();
      this.notice = `Final draft test passed: ${probeSummary(this.probe)}`;
    });
  };
  activateWizardProvider = async () => {
    const provider = this.wizardProvider;
    if (!provider) return;
    await this.run('activate', async () => {
      const generation = await activateProvider(provider);
      await this.refetchWizardModels();
      this.wizardStep = 3;
      this.notice = `Provider activated in runtime generation ${generation}.`;
      await Promise.all([
        this.queryClient.invalidateQueries({
          queryKey: providerKeys.summaries
        }),
        this.queryClient.invalidateQueries({
          queryKey: providerKeys.modelCatalog
        }),
        this.queryClient.invalidateQueries({ queryKey: overviewKeys.root })
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
      if (!copy || this.copyLoaded || !this.providerKinds.data) return;
      this.copyLoaded = true;
      void getProvider(copy)
        .then((source) => {
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
          this.errorMessage = message(error);
        });
    });
    onDestroy(() => {
      void this.queryClient.cancelQueries({
        queryKey: providerKeys.modelsOf(this.providerId)
      });
      if (this.draft) this.draft.credential = '';
      this.grantInput = '';
    });
  }
}
