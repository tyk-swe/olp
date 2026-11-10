import { untrack } from 'svelte';
import {
  createQuery,
  keepPreviousData,
  useQueryClient
} from '@tanstack/svelte-query';
import { useRole } from '#lib/features/access/session/useRole.svelte.js';
import {
  useMutationIntent,
  usePrincipalLifetime
} from '#lib/editing/mutationIntent.svelte.js';
import {
  acceptRemote,
  conflictNotice,
  initialConcurrentEdit,
  markConflict,
  markDirty,
  markSaved,
  reconcile
} from '#lib/editing/concurrentEdit.js';
import { emptyCursorHistory, resetCursor } from '#lib/listing/pagination.js';
import {
  errorMessage,
  fieldIssues,
  isEtagMismatch,
  type FieldIssue
} from '#lib/management/http.js';
import {
  activateProvider,
  disableProvider,
  isProviderInUse,
  probeProvider,
  restoreProviderAsDraft,
  updateProvider,
  type Provider
} from './providers';
import {
  listProviderKinds,
  listProviderModelPage,
  type ProviderModelPage
} from './models';
import {
  revokeProviderCredential,
  rotateProviderCredential,
  type ExternalCredentialReference,
  type ProviderCredential
} from './credentials';
import { diffProviderRevisions, restoreProviderRevision } from './revisions';
import { acceptCatalogSuggestions } from './catalog';
import {
  createNetworkCredential,
  revokeNetworkCredential
} from './providerProfiles';
import { nativeObject, parseNativeJSON } from '#lib/nativeJson.js';
import { networkCredentialPath } from './configurationDraft.svelte';
import {
  certifyProviderModel,
  declareProviderModels,
  discoverProviderModels,
  setProviderModel,
  type CapabilityDeclaration
} from './models';
import { providerKeys } from './providerKeys';
import {
  refreshProviderChange,
  refreshProviderReceipt,
  type ProviderChangeReceipt
} from './providerChanges';
import {
  buildUpdateProviderInput,
  disableNotice,
  providerEditValues,
  probeSummary,
  certificationPrerequisiteReady,
  parseManualModelNames,
  type ProviderEditValues
} from './providerEditor';

export class ProviderEditor {
  private readonly queryClient = useQueryClient();
  private readonly access = useRole();
  readonly canManage = $derived(this.access.can('providers.manage'));
  readonly modelPageState = $state(emptyCursorHistory());
  readonly snapshot;
  readonly providerKinds = createQuery(() => ({
    queryKey: providerKeys.kinds(),
    queryFn: ({ signal }) => listProviderKinds(signal)
  }));
  readonly providerSpec = $derived(
    this.providerKinds.data?.find(
      (candidate) => candidate.kind === this.current?.configuration.kind
    )
  );
  private busy = $state('');
  private editVersion = 0;
  private error = $state('');
  private validationIssues = $state<FieldIssue[]>([]);
  private notice = $state('');
  private reloadVersion = $state(0);
  private explicitReloadEpoch = $state(0);
  private savedDraftEpoch = $state(0);
  private referenceConflict = $state('');
  private pending = $state<ProviderChangeReceipt | null>(null);
  private refreshError = $state('');
  private editValues = $state<ProviderEditValues>({
    name: '',
    endpoint: '',
    apiVersion: '',
    cloudRegion: '',
    cloudProject: '',
    deployment: '',
    authMode: 'api_key'
  });
  private sync = $state(initialConcurrentEdit());
  private readonly activateIntent = useMutationIntent();
  private readonly disableIntent = useMutationIntent();
  private readonly restoreIntent = useMutationIntent();
  private readonly revisionIntent = useMutationIntent();
  private readonly stageIntent = useMutationIntent();
  private readonly revokeIntent = useMutationIntent();
  private readonly networkCreateIntent = useMutationIntent();
  private readonly networkRevokeIntent = useMutationIntent();
  private readonly catalogIntent = useMutationIntent();
  private identity: string;
  private resetIntents = () => {
    this.activateIntent.reset();
    this.disableIntent.reset();
    this.restoreIntent.reset();
    this.revisionIntent.reset();
    this.stageIntent.reset();
    this.revokeIntent.reset();
    this.networkCreateIntent.reset();
    this.networkRevokeIntent.reset();
    this.catalogIntent.reset();
  };
  private reset = () => {
    this.busy =
      this.error =
      this.notice =
      this.refreshError =
      this.referenceConflict =
        '';
    this.validationIssues = [];
    this.pending = null;
    this.sync = initialConcurrentEdit();
    this.editValues = {
      name: '',
      endpoint: '',
      apiVersion: '',
      cloudRegion: '',
      cloudProject: '',
      deployment: '',
      authMode: 'api_key'
    };
    this.explicitReloadEpoch += 1;
    this.resetIntents();
    this.resetModelPage();
  };
  private readonly principal = usePrincipalLifetime(this.reset);
  private captureOwner = () => {
    const principal = this.principal();
    const id = this.providerId();
    const epoch = this.explicitReloadEpoch;
    return () =>
      principal() &&
      this.providerId() === id &&
      this.explicitReloadEpoch === epoch;
  };
  private get current() {
    const current = this.snapshot.data?.provider;
    return current?.id === this.providerId() ? current : undefined;
  }
  get view() {
    return {
      busy: this.busy,
      error: this.error,
      notice: this.notice,
      validationIssues: this.validationIssues,
      refreshError: this.refreshError,
      refreshPending: Boolean(this.pending),
      referenceConflict: this.referenceConflict,
      draft: this.editValues,
      dirty: this.sync.dirty,
      concurrentNotice: conflictNotice(this.sync),
      reloadVersion: this.reloadVersion,
      explicitReloadEpoch: this.explicitReloadEpoch,
      savedDraftEpoch: this.savedDraftEpoch
    } as const;
  }
  readonly touch = () => {
    this.editVersion += 1;
    this.sync = markDirty(this.sync);
  };
  readonly reportError = (message: string) => {
    this.error = message;
    this.validationIssues = [];
    this.notice = '';
  };
  readonly reportNotice = (message: string) => {
    this.error = '';
    this.validationIssues = [];
    this.notice = message;
  };
  readonly markConflict = () => {
    this.sync = markConflict(this.sync);
  };
  readonly resetModelPage = () => {
    resetCursor(this.modelPageState);
  };
  private async run<T>(
    label: string,
    action: (isCurrent: () => boolean) => Promise<T>
  ): Promise<T | undefined> {
    if (this.busy) return;
    const isCurrent = this.captureOwner();
    this.busy = label;
    this.error = this.notice = this.referenceConflict = '';
    this.validationIssues = [];
    try {
      const result = await action(isCurrent);
      return isCurrent() ? result : undefined;
    } catch (error) {
      if (!isCurrent()) return;
      if (isEtagMismatch(error)) this.sync = markConflict(this.sync);
      else {
        this.error = errorMessage(error);
        this.validationIssues = fieldIssues(error);
      }
      return;
    } finally {
      if (isCurrent()) this.busy = '';
    }
  }
  private readSnapshot = async (mutation?: {
    previousEtag: string;
    etag: string;
  }) => {
    const isCurrent = this.captureOwner();
    const acknowledged =
      mutation &&
      !this.sync.conflict &&
      this.sync.snapshotEtag === mutation.previousEtag &&
      this.sync.remoteEtag === mutation.previousEtag;
    const result = await this.snapshot.refetch();
    if (!isCurrent()) return;
    if (result.error) throw result.error;
    if (!result.data) throw new Error('The provider snapshot is unavailable.');
    this.sync =
      acknowledged && result.data.provider.etag === mutation?.etag
        ? markSaved(mutation.etag, this.sync.dirty)
        : acceptRemote(this.sync, result.data.provider.etag);
  };
  readonly refresh = async () => {
    const isCurrent = this.captureOwner();
    await this.readSnapshot();
    if (!isCurrent()) return;
    const result = await refreshProviderChange(this.queryClient, {
      kind: 'details'
    });
    if (isCurrent() && result.kind === 'refresh-failed')
      throw new Error(result.message);
  };
  readonly reload = async () => {
    await this.run('reload', async (isCurrent) => {
      const result = await this.snapshot.refetch();
      if (!isCurrent()) return;
      if (result.error) throw result.error;
      if (!result.data)
        throw new Error('The provider snapshot is unavailable.');
      this.sync = markSaved(result.data.provider.etag, false);
      if (this.providerSpec)
        this.editValues = providerEditValues(
          result.data.provider,
          this.providerSpec
        );
      this.busy = '';
      this.pending = null;
      this.refreshError = '';
      this.resetIntents();
      this.explicitReloadEpoch += 1;
      this.reloadVersion += 1;
    });
  };
  private async observe(
    receipt: ProviderChangeReceipt,
    isCurrent: () => boolean
  ) {
    if (!isCurrent()) return;
    this.pending = receipt;
    this.notice = receipt.notice;
    this.refreshError = '';
    const result = await refreshProviderReceipt(
      this.queryClient,
      receipt,
      () => this.readSnapshot(receipt.snapshot),
      isCurrent
    );
    if (result.kind === 'stale') return;
    if (result.kind === 'refresh-failed') {
      this.refreshError = result.message;
      return;
    }
    this.pending = null;
    this.reportNotice(receipt.notice);
  }
  readonly retryRefresh = async () => {
    const receipt = this.pending;
    if (receipt)
      await this.run('refresh', (isCurrent) =>
        this.observe(receipt, isCurrent)
      );
  };
  readonly saveDraft = async () => {
    const current = this.current;
    const spec = this.providerSpec;
    if (!current || !spec || !this.canManage || this.pending) return false;
    return this.run('save', async (isCurrent) => {
      if (!this.sync.snapshotEtag)
        throw new Error('Reload the provider before saving.');
      const version = this.editVersion;
      const updated = await updateProvider(
        current.id,
        this.sync.snapshotEtag,
        buildUpdateProviderInput(this.editValues, spec)
      );
      if (!isCurrent()) return;
      this.sync = markSaved(updated.etag, this.editVersion !== version);
      this.savedDraftEpoch += 1;
      this.activateIntent.reset();
      this.disableIntent.reset();
      this.restoreIntent.reset();
      this.queryClient.setQueriesData<ProviderModelPage>(
        { queryKey: providerKeys.modelsOf(current.id) },
        (page) => (page ? { ...page, provider: updated } : page)
      );
      this.resetModelPage();
      await this.observe(
        {
          change: { kind: 'activation', id: current.id },
          notice: this.sync.dirty
            ? 'Draft saved. You have additional unsaved changes.'
            : 'Provider draft settings saved.'
        },
        isCurrent
      );
    });
  };
  readonly probe = async () => {
    const current = this.current;
    if (!current || !this.canManage || this.pending || this.sync.dirty)
      return false;
    return this.run('detail-probe', async (isCurrent) => {
      const { result } = await probeProvider(current);
      if (!isCurrent()) return;
      if (!result.succeeded) throw new Error(result.detail);
      await this.observe(
        {
          change: { kind: 'activation', id: current.id },
          notice: `Connection succeeded: ${probeSummary(result)}`
        },
        isCurrent
      );
    });
  };
  readonly activate = async () => {
    const current = this.current;
    if (!current || !this.canManage || this.pending || this.sync.dirty)
      return false;
    return this.run('detail-activate', async (isCurrent) => {
      const submission = this.activateIntent.prepare(current.id, () => ({
        id: current.id,
        etag: current.etag
      }));
      const generation = await activateProvider(
        submission.input,
        submission.requestId
      );
      if (!isCurrent()) return;
      this.activateIntent.reset();
      await this.observe(
        {
          change: { kind: 'activation', id: current.id },
          notice: `Activated in runtime generation ${generation}.`
        },
        isCurrent
      );
    });
  };
  readonly disable = async () => {
    const current = this.current;
    if (!current || !this.canManage || this.pending) return false;
    return this.run('detail-disable', async (isCurrent) => {
      const submission = this.disableIntent.prepare(current.id, () => ({
        id: current.id,
        etag: current.etag
      }));
      try {
        const generation = await disableProvider(
          submission.input,
          submission.requestId
        );
        if (!isCurrent()) return;
        this.disableIntent.reset();
        await this.observe(
          {
            change: { kind: 'activation', id: current.id },
            notice: disableNotice(generation)
          },
          isCurrent
        );
      } catch (error) {
        if (!isCurrent()) return;
        if (!isProviderInUse(error)) throw error;
        this.referenceConflict = errorMessage(error);
      }
    });
  };
  readonly restoreDraft = async () => {
    const current = this.current;
    if (!current || !this.canManage || this.pending) return false;
    return this.run('detail-restore-draft', async (isCurrent) => {
      const submission = this.restoreIntent.prepare(current.id, () => ({
        id: current.id,
        etag: current.etag
      }));
      await restoreProviderAsDraft(submission.input, submission.requestId);
      if (!isCurrent()) return;
      this.restoreIntent.reset();
      await this.observe(
        {
          change: { kind: 'activation', id: current.id },
          notice:
            'Provider restored as a draft. Stored capabilities are declared again: re-certify and test before activation.'
        },
        isCurrent
      );
    });
  };
  readonly stageCredential = async (
    credential: string | ExternalCredentialReference
  ) => {
    const current = this.current;
    if (!current || !this.canManage || this.pending) return false;
    return this.run('rotate-credential', async (isCurrent) => {
      const submission = this.stageIntent.prepare(
        { providerId: current.id, credential },
        () => [{ id: current.id, etag: current.etag }, credential] as const
      );
      await rotateProviderCredential(...submission.input, submission.requestId);
      if (!isCurrent()) return;
      this.stageIntent.reset();
      this.resetModelPage();
      await this.observe(
        {
          change: { kind: 'activation', id: current.id },
          notice:
            'Credential version staged. Test and activate the provider to publish it; the current runtime credential remains live until then.'
        },
        isCurrent
      );
      return true;
    });
  };
  readonly revokeCredential = async (credential: ProviderCredential) => {
    const current = this.current;
    if (!current || !this.canManage || this.pending) return false;
    return this.run(`revoke-${credential.id}`, async (isCurrent) => {
      const submission = this.revokeIntent.prepare(
        { providerId: current.id, credentialId: credential.id },
        () => [{ id: current.id, etag: current.etag }, credential.id] as const
      );
      await revokeProviderCredential(...submission.input, submission.requestId);
      if (!isCurrent()) return;
      this.revokeIntent.reset();
      await this.observe(
        {
          change: { kind: 'activation', id: current.id },
          notice: `Credential version ${credential.version} revoked.`
        },
        isCurrent
      );
    });
  };
  readonly restoreRevision = async (revisionId: string, revision: number) => {
    const current = this.current;
    if (!current || !this.canManage || this.pending) return false;
    return this.run('revision-restore', async (isCurrent) => {
      const submission = this.revisionIntent.prepare(
        { providerId: current.id, revisionId },
        () => ({ provider: { id: current.id, etag: current.etag }, revisionId })
      );
      await restoreProviderRevision(
        submission.input.provider,
        submission.input.revisionId,
        submission.requestId
      );
      if (!isCurrent()) return;
      this.revisionIntent.reset();
      this.resetModelPage();
      await this.observe(
        {
          change: { kind: 'activation', id: current.id },
          notice: `Revision ${revision} restored as a new draft. No historical credential was restored; the current credential selection was preserved. Test and certify before activation.`
        },
        isCurrent
      );
      return true;
    });
  };
  readonly compareRevisions = async (from: string, to: string) => {
    const current = this.current;
    if (!current) return;
    return this.run('revision-diff', () =>
      diffProviderRevisions(current.id, from, to)
    );
  };
  readonly discoverModels = async () => {
    const current = this.current;
    if (!current || !this.canManage || this.pending) return;
    return this.run('detail-discover', async (isCurrent) => {
      const updated = await discoverProviderModels(current);
      if (!isCurrent()) return;
      this.resetModelPage();
      await this.observe(
        {
          change: { kind: 'activation', id: current.id },
          notice: `${updated.model_count} model${updated.model_count === 1 ? '' : 's'} discovered. Review capabilities before activation.`
        },
        isCurrent
      );
      return true;
    });
  };
  readonly declareModels = async (input: string) => {
    const current = this.current;
    if (!current || !this.canManage || this.pending) return;
    const names = parseManualModelNames(input);
    if (!names.length) {
      this.reportError('Enter at least one upstream model identifier.');
      return;
    }
    return this.run('detail-declare', async (isCurrent) => {
      const updated = await declareProviderModels(current, names);
      if (!isCurrent()) return;
      this.resetModelPage();
      await this.observe(
        {
          change: { kind: 'activation', id: current.id },
          notice: `${updated.model_count} manually declared model${updated.model_count === 1 ? '' : 's'} ready for capability review.`
        },
        isCurrent
      );
      return true;
    });
  };
  readonly reviewModel = async (
    provider: Provider,
    modelId: string,
    enabled: boolean,
    capabilities: CapabilityDeclaration[]
  ) => {
    if (!this.canManage || this.pending || provider.id !== this.providerId())
      return false;
    return this.run(`model-${modelId}`, async (isCurrent) => {
      await setProviderModel(provider, modelId, enabled, capabilities);
      if (!isCurrent()) return false;
      await this.observe(
        {
          change: { kind: 'activation', id: provider.id },
          notice: 'Capability review saved with declared provenance.'
        },
        isCurrent
      );
      return true;
    });
  };
  readonly certifyModel = async (provider: Provider, modelId: string) => {
    if (!this.canManage || this.pending || provider.id !== this.providerId())
      return;
    return this.run(`certify-${modelId}`, async (isCurrent) => {
      const probe = certificationPrerequisiteReady(provider)
        ? undefined
        : (await probeProvider(provider)).result;
      if (!isCurrent()) return;
      if (probe && !probe.succeeded) throw new Error(probe.detail);
      const tested = probe
        ? {
            ...provider,
            last_probe_at: probe.checked_at,
            last_probe_status: 'succeeded',
            last_probe_detail: probe.detail
          }
        : provider;
      const { result } = await certifyProviderModel(tested, modelId);
      if (!isCurrent()) return;
      await this.observe(
        {
          change: { kind: 'activation', id: provider.id },
          notice: `${result.certified_count} of ${result.attempted_count} reviewed tuples passed server certification. Test the completed draft before activation.`
        },
        isCurrent
      );
      return result;
    });
  };
  readonly acceptCatalog = async (digest: string, models: string[]) => {
    const current = this.current;
    if (!current || !this.canManage || this.pending || !models.length) return;
    return this.run('catalog-accept', async (isCurrent) => {
      const submission = this.catalogIntent.prepare(
        { providerId: current.id, digest, models },
        () => [{ id: current.id, etag: current.etag }, digest, models] as const
      );
      await acceptCatalogSuggestions(...submission.input, submission.requestId);
      if (!isCurrent()) return;
      this.catalogIntent.reset();
      const count = submission.input[2].length;
      await this.observe(
        {
          change: { kind: 'activation', id: current.id },
          notice: `Catalog facts stored for ${count} model${count === 1 ? '' : 's'}. Certify the provider's capabilities again before activation.`
        },
        isCurrent
      );
      return true;
    });
  };
  readonly stageNetworkCredential = async (secret: string) => {
    const current = this.current;
    if (!current || !this.canManage || this.pending) return;
    return this.run('network-credential', async (isCurrent) => {
      if (!nativeObject(parseNativeJSON(secret)))
        throw new Error('Enter a network credential JSON object.');
      const submission = this.networkCreateIntent.prepare(
        { providerId: current.id, secret },
        () => [current.id, current.etag, secret] as const
      );
      const created = await createNetworkCredential(
        ...submission.input,
        submission.requestId
      );
      if (!isCurrent()) return;
      this.networkCreateIntent.reset();
      this.editValues.document?.set(
        networkCredentialPath,
        created.credential_id
      );
      this.touch();
      await this.observe(
        {
          change: { kind: 'activation', id: current.id },
          snapshot: { previousEtag: submission.input[1], etag: created.etag },
          notice: 'Network credential stored and selected in the draft.'
        },
        isCurrent
      );
      return true;
    });
  };
  readonly revokeNetworkCredential = async (credentialId: string) => {
    const current = this.current;
    if (!current || !this.canManage || this.pending) return;
    return this.run('revoke-network-credential', async (isCurrent) => {
      const submission = this.networkRevokeIntent.prepare(
        { providerId: current.id, credentialId },
        () => [current.id, current.etag, credentialId] as const
      );
      const revoked = await revokeNetworkCredential(
        ...submission.input,
        submission.requestId
      );
      if (!isCurrent()) return;
      this.networkRevokeIntent.reset();
      await this.observe(
        {
          change: { kind: 'activation', id: current.id },
          snapshot: { previousEtag: submission.input[1], etag: revoked.etag },
          notice: 'Network credential revoked.'
        },
        isCurrent
      );
    });
  };
  constructor(private readonly providerId: () => string) {
    this.identity = providerId();
    $effect(() => {
      const id = providerId();
      if (id === this.identity) return;
      this.identity = id;
      untrack(this.reset);
    });
    this.snapshot = createQuery(() => {
      const id = providerId();
      const cursor = this.modelPageState.cursor;
      return {
        queryKey: providerKeys.models(id, cursor),
        queryFn: ({ signal }) => listProviderModelPage(id, cursor, signal),
        enabled: Boolean(id),
        placeholderData: keepPreviousData
      };
    });
    $effect(() => {
      const value = this.snapshot.data?.provider;
      if (!value || value.id !== this.providerId() || !this.providerSpec)
        return;
      const next = reconcile(this.sync, value.etag);
      if (next.state !== this.sync) this.sync = next.state;
      if (next.hydrate)
        this.editValues = providerEditValues(value, this.providerSpec);
    });
  }
}
