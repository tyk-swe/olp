import { createQuery, useQueryClient } from '@tanstack/svelte-query';
import { SvelteURLSearchParams } from 'svelte/reactivity';
import { MutationIntent } from '#lib/editing/mutationIntent.js';
import { usePrincipalLifetime } from '#lib/editing/mutationIntent.svelte.js';
import { captureResult, readResult } from '#lib/editing/result.js';
import { errorMessage, fieldIssues } from '#lib/management/http.js';
import { stringifyNativeJSON } from '#lib/nativeJson.js';
import { networkCredentialPath } from './configurationDraft.svelte';
import { listProviderCredentials } from './credentials';
import { GrantEnrollmentState } from './grantEnrollment.svelte';
import { refreshProviderReceipt } from './providerChanges';
import {
  buildCreateProviderInput,
  createProviderDraft,
  validateProviderDraft,
  type ProviderDraft
} from './providerEditor';
import { providerKeys } from './providerKeys';
import type { ProviderProfile } from './providerProfiles';
import {
  getProvider,
  probeProvider,
  type Provider,
  type ProviderVendor
} from './providers';
import {
  isLiveGrant,
  isRefusedProblem,
  resolveConnectionProgress,
  saveProviderConnection,
  type ConnectionProgress
} from './providerWizardConnection';
import type { ProviderWizardState } from './providerWizard.svelte';

export class ProviderWizardConnectionStage {
  private readonly queryClient = useQueryClient();
  private readonly createIntent = new MutationIntent();
  private readonly rotateIntent = new MutationIntent();
  private progress?: ConnectionProgress;
  private copyLoaded = false;
  private readonly capturePrincipal = usePrincipalLifetime(() => this.reset());
  readonly grant;
  readonly draftCredentials;
  readonly grantEnrolled = $derived.by(() =>
    isLiveGrant(
      this.draftCredentials.data,
      this.wizard.wizardProvider?.draft_credential_id,
      this.wizard.draft?.profileRevision
    )
  );

  constructor(private readonly wizard: ProviderWizardState) {
    const principal = wizard.access.user?.id;
    this.grant = new GrantEnrollmentState({
      allowed: () => wizard.canManage && wizard.access.user?.id === principal,
      error: (message, failure) => {
        if (wizard.claimConflict(failure)) return;
        wizard.action.error = message;
        wizard.action.issues = fieldIssues(failure);
      },
      completed: async (_completion, { isCurrent }) => {
        wizard.notice =
          'Grant enrolled. Testing the connection remains a separate step.';
        await this.testEnrolledGrant(isCurrent);
      }
    });
    this.draftCredentials = createQuery(() => ({
      queryKey: providerKeys.credentials(wizard.providerId),
      queryFn: ({ signal }) =>
        listProviderCredentials(wizard.providerId, signal),
      enabled:
        wizard.grantRequired &&
        Boolean(wizard.wizardProvider?.draft_credential_id)
    }));
    $effect(() => {
      this.loadCopiedProvider();
    });
  }

  reset() {
    this.progress = undefined;
    this.createIntent.reset();
    this.rotateIntent.reset();
    const draft = this.wizard.draft;
    if (draft) {
      draft.credential = '';
      draft.externalCredentialReference = '';
    }
    this.grant.input = '';
  }

  save = async (event: SubmitEvent) => {
    event.preventDefault();
    const { wizard } = this;
    const draft = wizard.draft;
    const spec = wizard.selectedSpec;
    if (!wizard.canManage || wizard.busy || !draft || !spec) return;
    const existing = wizard.wizardProvider;
    const issue = validateProviderDraft(draft, spec, {
      credentialAlreadyStored: Boolean(
        existing || this.progress?.phase === 'saved'
      ),
      profiles: this.queryClient.getQueryData<ProviderProfile[]>(
        providerKeys.profiles()
      ),
      vendors: this.queryClient.getQueryData<ProviderVendor[]>(
        providerKeys.vendors()
      )
    });
    if (issue) {
      wizard.action.error = issue;
      wizard.action.issues = [];
      return;
    }
    const resolved = resolveConnectionProgress(
      buildCreateProviderInput(draft, spec),
      this.progress,
      existing
    );
    if (resolved.error || !resolved.progress) {
      wizard.action.error = resolved.error ?? '';
      return;
    }
    const progress = resolved.progress;
    if (!this.progress) wizard.activation.reset();
    this.progress = progress;
    await wizard.action.run(
      async (current) => {
        const outcome = await readResult(() =>
          this.connect(progress, draft, current)
        );
        if (outcome.ok || !current()) return;
        this.settleFailedSave(outcome.error);
        throw outcome.error;
      },
      undefined,
      { key: 'create' }
    );
  };

  private settleFailedSave(cause: unknown) {
    const progress = this.progress;
    if (progress?.phase !== 'create' && progress?.phase !== 'update') return;
    if (!isRefusedProblem(cause)) {
      this.progress = { ...progress, uncertain: true };
      return;
    }
    if (progress.uncertain) return;
    this.progress = undefined;
    this.createIntent.reset();
  }

  private connect = async (
    progress: ConnectionProgress,
    draft: ProviderDraft,
    current: () => boolean
  ) => {
    const { wizard } = this;
    const submission = await saveProviderConnection(progress, {
      createIntent: this.createIntent,
      rotateIntent: this.rotateIntent,
      isCurrent: current,
      setProgress: (next) => {
        this.progress = next;
      },
      setNotice: (notice) => {
        wizard.notice = notice;
      }
    });
    if (!submission || !current()) return;
    draft.credential = '';
    wizard.providerId = submission.id;
    const notice =
      'Provider saved. Connection testing remains a separate step.';
    wizard.notice = notice;
    const refreshed = await refreshProviderReceipt(
      this.queryClient,
      { change: { kind: 'connection' }, notice },
      wizard.models.refetch,
      current
    );
    if (refreshed.kind === 'stale') return;
    if (refreshed.kind === 'refresh-failed') {
      wizard.action.error = `The provider change succeeded, but refreshing failed. ${refreshed.message}`;
      return;
    }
    const provider = refreshed.value.provider;
    if (wizard.grantRequired) {
      const enrolled = await this.holdsLiveGrant(provider);
      if (!current()) return;
      if (!enrolled) {
        await this.grant.start(provider);
        if (current() && this.grant.enrollment) this.progress = undefined;
        return;
      }
    }
    await this.testConnection(provider, current);
    if (current()) this.progress = undefined;
  };

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

  private testEnrolledGrant = async (current: () => boolean) => {
    await this.queryClient.invalidateQueries(
      { queryKey: providerKeys.credentials(this.wizard.providerId) },
      { throwOnError: true }
    );
    if (!current()) return;
    const snapshot = await this.wizard.models.refetch();
    if (current()) await this.testConnection(snapshot.provider, current);
  };

  private testConnection = async (
    provider: Provider,
    current: () => boolean
  ) => {
    const { result: probe } = await probeProvider(provider);
    if (!current()) return;
    this.wizard.probe = probe;
    if (!probe.succeeded) throw new Error(probe.detail);
    this.wizard.wizardStep = 2;
  };

  private loadCopiedProvider() {
    const { wizard } = this;
    const copy = globalThis.location
      ? new SvelteURLSearchParams(globalThis.location.search).get('copy')
      : null;
    if (!copy || this.copyLoaded) return;
    const initialDraft = wizard.draft;
    const initialSpec = wizard.selectedSpec;
    if (!initialDraft || !initialSpec) return;
    this.copyLoaded = true;
    const samePrincipal = this.capturePrincipal();
    const initialInput = stringifyNativeJSON(
      buildCreateProviderInput(initialDraft, initialSpec)
    );
    const canApplyCopy = () => {
      const spec = wizard.selectedSpec;
      if (
        !samePrincipal() ||
        wizard.providerId ||
        this.progress ||
        wizard.draft !== initialDraft ||
        !spec
      )
        return false;
      const unchanged = captureResult(
        () =>
          initialInput ===
          stringifyNativeJSON(buildCreateProviderInput(initialDraft, spec))
      );
      return unchanged.ok && unchanged.value;
    };
    void getProvider(copy)
      .then((source) => {
        if (!canApplyCopy()) return;
        const spec = wizard.providerKinds.data?.find(
          (kind) => kind.kind === source.configuration.kind
        );
        if (!spec) return;
        const duplicate = createProviderDraft(spec, source.configuration);
        duplicate.name = `${source.name} copy`;
        duplicate.credential = '';
        // Network credentials are provider-owned and cannot be reused by a copy.
        if (duplicate.document?.at(networkCredentialPath) !== undefined) {
          duplicate.document.set(networkCredentialPath, undefined);
          wizard.notice =
            'The copied connection needs its own network credential. The source credential reference was removed.';
        }
        wizard.draft = duplicate;
      })
      .catch((error) => {
        if (canApplyCopy()) wizard.action.error = errorMessage(error);
      });
  }
}
