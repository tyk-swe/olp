import { untrack } from 'svelte';
import {
  createQuery,
  keepPreviousData,
  useQueryClient
} from '@tanstack/svelte-query';
import { useRole } from '#lib/session/useRole.svelte.js';
import {
  EditorSession,
  useMutationIntentGroup
} from '#lib/editing/editorSession.svelte.js';
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
import { listProviderModelPage, type CapabilityDeclaration } from './models';
import { providerKeys } from '#lib/domain/providers/providerKeys.js';
import {
  refreshProviderChange,
  refreshProviderReceipt,
  type ProviderChangeReceipt
} from './providerChanges';
import { providerEditValues, type ProviderEditValues } from './providerEditor';
import type { ExternalCredentialReference } from './credentials';
import { networkCredentialPath } from './configurationDraft.svelte';
import type { ProviderWrite } from './providerWrite';
import * as lifecycleActions from './providerLifecycleActions';
import * as credentialActions from './providerCredentialActions';
import * as modelActions from './providerModelActions';
import * as revisionActions from './providerRevisionActions';
import { providerQueries } from '#lib/domain/providers/queries.js';
import type {
  Provider,
  ProviderCredential
} from '#lib/domain/providers/api.js';

const providerEditorIntentNames = [
  'activate',
  'disable',
  'restore',
  'revision',
  'stage',
  'revoke',
  'networkCreate',
  'networkRevoke',
  'catalog'
] as const;

function emptyEditValues(): ProviderEditValues {
  return {
    name: '',
    endpoint: '',
    apiVersion: '',
    cloudRegion: '',
    cloudProject: '',
    deployment: '',
    authMode: 'api_key'
  };
}

export class ProviderEditor {
  private readonly queryClient = useQueryClient();
  private readonly access = useRole();
  readonly canManage = $derived(this.access.can('providers.manage'));
  readonly modelPageState = $state(emptyCursorHistory());
  readonly snapshot;
  readonly providerKinds = createQuery(providerQueries.kinds);
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
  private editValues = $state<ProviderEditValues>(emptyEditValues());
  private sync = $state(initialConcurrentEdit());
  private readonly intents = useMutationIntentGroup(providerEditorIntentNames);
  private identity: string;

  private readonly reset = () => {
    this.busy =
      this.error =
      this.notice =
      this.refreshError =
      this.referenceConflict =
        '';
    this.validationIssues = [];
    this.pending = null;
    this.sync = initialConcurrentEdit();
    this.editValues = emptyEditValues();
    this.explicitReloadEpoch += 1;
    this.intents.resetAll();
    this.resetModelPage();
  };

  private readonly session = new EditorSession(this.reset);

  private captureOwner = () => {
    const id = this.providerId();
    const epoch = this.explicitReloadEpoch;
    return this.session.capture(
      () => this.providerId() === id && this.explicitReloadEpoch === epoch
    ).isCurrent;
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

  private run = async <T>(
    label: string,
    action: (isCurrent: () => boolean) => Promise<T>
  ): Promise<T | undefined> => {
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
  };

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
      this.intents.resetAll();
      this.explicitReloadEpoch += 1;
      this.reloadVersion += 1;
    });
  };

  private observe = async (
    receipt: ProviderChangeReceipt,
    isCurrent: () => boolean
  ) => {
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
  };

  readonly retryRefresh = async () => {
    const receipt = this.pending;
    if (receipt)
      await this.run('refresh', (isCurrent) =>
        this.observe(receipt, isCurrent)
      );
  };

  private readonly write: ProviderWrite = {
    writable: () =>
      this.canManage && !this.pending ? this.current : undefined,
    canWrite: (provider) =>
      this.canManage && !this.pending && provider.id === this.providerId(),
    run: this.run,
    observe: this.observe,
    resetModelPage: this.resetModelPage
  };

  private readonly markDraftSaved = (etag: string, stillDirty: boolean) => {
    this.sync = markSaved(etag, stillDirty);
    this.savedDraftEpoch += 1;
    this.intents.reset('activate', 'disable', 'restore');
  };

  private readonly showReferenceConflict = (message: string) => {
    this.referenceConflict = message;
  };

  private readonly selectNetworkCredential = (credentialId: string) => {
    this.editValues.document?.set(networkCredentialPath, credentialId);
    this.touch();
  };

  readonly saveDraft = () =>
    lifecycleActions.saveDraft(this.write, this.queryClient, {
      spec: this.providerSpec,
      snapshotEtag: this.sync.snapshotEtag,
      values: this.editValues,
      version: () => this.editVersion,
      onSaved: this.markDraftSaved
    });
  readonly probe = () => lifecycleActions.probe(this.write, this.sync.dirty);
  readonly activate = () =>
    lifecycleActions.activate(
      this.write,
      this.intents.activate,
      this.sync.dirty
    );
  readonly disable = () =>
    lifecycleActions.disable(
      this.write,
      this.intents.disable,
      this.showReferenceConflict
    );
  readonly restoreDraft = () =>
    lifecycleActions.restoreAsDraft(this.write, this.intents.restore);

  readonly stageCredential = (
    credential: string | ExternalCredentialReference
  ) =>
    credentialActions.stageCredential(
      this.write,
      this.intents.stage,
      credential
    );
  readonly revokeCredential = (credential: ProviderCredential) =>
    credentialActions.revokeCredential(
      this.write,
      this.intents.revoke,
      credential
    );
  readonly stageNetworkCredential = (secret: string) =>
    credentialActions.stageNetworkCredential(
      this.write,
      this.intents.networkCreate,
      secret,
      this.selectNetworkCredential
    );
  readonly revokeNetworkCredential = (credentialId: string) =>
    credentialActions.retireNetworkCredential(
      this.write,
      this.intents.networkRevoke,
      credentialId
    );

  readonly compareListing = () => modelActions.compareListing(this.write);
  readonly discoverModels = () => modelActions.discoverModels(this.write);
  readonly declareModels = (input: string) =>
    modelActions.declareModels(this.write, input, this.reportError);
  readonly reviewModel = (
    provider: Provider,
    modelId: string,
    enabled: boolean,
    capabilities: CapabilityDeclaration[]
  ) =>
    modelActions.reviewModel(this.write, provider, {
      modelId,
      enabled,
      capabilities
    });
  readonly certifyModel = (provider: Provider, modelId: string) =>
    modelActions.certifyModel(this.write, provider, modelId);
  readonly acceptCatalog = (digest: string, models: string[]) =>
    modelActions.acceptCatalog(
      this.write,
      this.intents.catalog,
      digest,
      models
    );

  readonly restoreRevision = (revisionId: string, revision: number) =>
    revisionActions.restoreRevision(this.write, this.intents.revision, {
      revisionId,
      revision
    });
  readonly compareRevisions = (from: string, to: string) =>
    revisionActions.compareRevisions(this.write, this.current, from, to);

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
