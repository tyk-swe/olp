import { untrack } from 'svelte';
import {
  createQuery,
  keepPreviousData,
  useQueryClient
} from '@tanstack/svelte-query';
import { useRole } from '#lib/session/useRole.svelte.js';
import { useAction } from '#lib/editing/action.svelte.js';
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
import { isEtagMismatch } from '#lib/management/http.js';
import { listProviderKinds, listProviderModelPage } from './models';
import { providerKeys } from './providerKeys';
import {
  refreshProviderChange,
  refreshProviderReceipt,
  type ProviderChangeReceipt
} from './providerChanges';
import { providerEditValues, type ProviderEditValues } from './providerEditor';
import {
  createProviderEditorActions,
  providerEditorIntentNames
} from './providerEditorActions';

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
  readonly providerKinds = createQuery(() => ({
    queryKey: providerKeys.kinds(),
    queryFn: ({ signal }) => listProviderKinds(signal)
  }));
  readonly providerSpec = $derived(
    this.providerKinds.data?.find(
      (candidate) => candidate.kind === this.current?.configuration.kind
    )
  );
  private editVersion = 0;
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
  private readonly action = useAction({
    current: () => this.providerId() === this.identity,
    conflict: (cause) => {
      if (!isEtagMismatch(cause)) return false;
      this.markConflict();
      return true;
    },
    begin: () => {
      this.notice = this.referenceConflict = '';
    }
  });

  private readonly reset = () => {
    this.action.retire();
    this.notice = this.refreshError = this.referenceConflict = '';
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
      busy: this.action.key,
      error: this.action.error,
      notice: this.notice,
      validationIssues: this.action.issues,
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
    this.action.error = message;
    this.notice = '';
  };
  readonly reportNotice = (message: string) => {
    this.action.reset();
    this.notice = message;
  };
  readonly markConflict = () => {
    this.sync = markConflict(this.sync);
  };
  readonly resetModelPage = () => {
    resetCursor(this.modelPageState);
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
    await this.action.run(
      async () => {
        const result = await this.snapshot.refetch();
        if (result.error) throw result.error;
        if (!result.data)
          throw new Error('The provider snapshot is unavailable.');
        return result.data.provider;
      },
      (provider) => {
        this.sync = markSaved(provider.etag, false);
        if (this.providerSpec)
          this.editValues = providerEditValues(provider, this.providerSpec);
        this.pending = null;
        this.refreshError = '';
        this.intents.resetAll();
        this.explicitReloadEpoch += 1;
        this.reloadVersion += 1;
      },
      { key: 'reload' }
    );
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
      await this.action.run(
        (isCurrent) => this.observe(receipt, isCurrent),
        undefined,
        { key: 'refresh' }
      );
  };

  private readonly actions = createProviderEditorActions({
    queryClient: this.queryClient,
    intents: this.intents,
    canManage: () => this.canManage,
    providerId: () => this.providerId(),
    current: () => this.current,
    providerSpec: () => this.providerSpec,
    pending: () => this.pending,
    dirty: () => this.sync.dirty,
    snapshotEtag: () => this.sync.snapshotEtag,
    editValues: () => this.editValues,
    editVersion: () => this.editVersion,
    markDraftSaved: (etag, stillDirty) => {
      this.sync = markSaved(etag, stillDirty);
      this.savedDraftEpoch += 1;
    },
    setReferenceConflict: (message) => {
      this.referenceConflict = message;
    },
    reportError: this.reportError,
    touch: this.touch,
    resetModelPage: this.resetModelPage,
    run: this.action.run,
    observe: this.observe
  });

  readonly saveDraft = this.actions.saveDraft;
  readonly probe = this.actions.probe;
  readonly activate = this.actions.activate;
  readonly disable = this.actions.disable;
  readonly restoreDraft = this.actions.restoreDraft;
  readonly stageCredential = this.actions.stageCredential;
  readonly revokeCredential = this.actions.revokeCredential;
  readonly restoreRevision = this.actions.restoreRevision;
  readonly compareListing = this.actions.compareListing;
  readonly compareRevisions = this.actions.compareRevisions;
  readonly discoverModels = this.actions.discoverModels;
  readonly declareModels = this.actions.declareModels;
  readonly reviewModel = this.actions.reviewModel;
  readonly certifyModel = this.actions.certifyModel;
  readonly acceptCatalog = this.actions.acceptCatalog;
  readonly stageNetworkCredential = this.actions.stageNetworkCredential;
  readonly revokeNetworkCredential = this.actions.revokeNetworkCredential;

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
