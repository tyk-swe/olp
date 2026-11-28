import { listProviderModelInventory } from '#lib/features/providers/index.js';
import { providerKeys } from '#lib/domain/providers/providerKeys.js';
import { routeKeys } from '#lib/domain/routes/routeKeys.js';
import { routeQueries } from '#lib/domain/routes/queries.js';
import { untrack } from 'svelte';
import { SvelteSet } from 'svelte/reactivity';
import { guardUnsavedChanges } from '#lib/editing/unsavedChanges.js';
import { createQuery, useQueryClient } from '@tanstack/svelte-query';
import { errorMessage, isEtagMismatch } from '#lib/management/http.js';
import {
  acceptRemote,
  beginReload,
  conflictNotice,
  initialConcurrentEdit,
  markConflict,
  markDirty,
  reconcile
} from '#lib/editing/concurrentEdit.js';
import {
  getRouteDraft,
  type RouteActivation,
  type RouteDraft,
  type RouteSimulation
} from '#lib/features/routes/api.js';
import { listPublications, agentKeys } from '#lib/features/agents/index.js';
import {
  discoveredOperations,
  discoveredSurfaces,
  inspectionDialects
} from '#lib/features/routes/inspectionDialects.js';
import { useRole } from '#lib/session/useRole.svelte.js';
import {
  emptyBehavior,
  parseSelectors,
  type RouteBehavior
} from '#lib/features/routes/routeBehavior.js';
import { modesFor } from '#lib/features/routes/routeOperations.js';
import {
  routeEligibilityWarnings as findRouteEligibilityWarnings,
  routeLifecycleWarnings as findRouteLifecycleWarnings,
  toRouteModelOptions,
  type EditableTarget
} from '#lib/features/routes/routeTargetModel.js';
import {
  EditorSession,
  useMutationIntentGroup
} from '#lib/editing/editorSession.svelte.js';
import {
  createDraft,
  openCreatedDraft,
  removeDraft,
  saveDraft,
  type RouteDraftPort
} from './routeDraftActions';
import {
  hydrateRouteBehavior,
  hydrateRouteTargets
} from './routeDraftHydration';
import {
  activateDraft,
  simulateDraft,
  simulationNotice,
  validateDraft
} from './routePublishActions';
import { validateRouteEditor } from './routeEditor';

export class RouteDraftEditorState {
  private readonly intents = useMutationIntentGroup(['activate', 'create']);
  readonly activateRouteIntent = this.intents.activate;
  readonly createIntent = this.intents.create;
  private createdDraftId = '';
  private readonly session = new EditorSession(() => {
    this.intents.resetAll();
  });

  readRouteId: () => string | undefined;
  get routeId() {
    return this.readRouteId();
  }
  resourceId = $derived(this.routeId ?? '');
  isNew = $derived(!this.resourceId);
  queryClient = useQueryClient();
  access = useRole();
  canManage = $derived(this.access.can('routes.manage'));
  draft;
  providerModels;
  toolsets;
  dialects = createQuery(() => routeQueries.dialects());
  operationOptions = $derived(
    discoveredOperations(this.dialects.data?.items ?? [])
  );
  surfaces = $derived.by(() =>
    discoveredSurfaces(
      this.simulationOperation,
      this.dialects.data?.items ?? []
    )
  );
  inspectionDialects = $derived.by(() =>
    inspectionDialects(
      this.simulationOperation,
      this.simulationSurface,
      this.dialects.data?.items ?? []
    )
  );
  modelOptions = $derived.by(() =>
    toRouteModelOptions(this.providerModels.data ?? [])
  );
  slug = $state('default');
  projectId = $state('');
  operations = $state<string[]>(['generation']);
  overallTimeoutMs = $state(120000);
  maxAttempts = $state(2);
  fidelity = $state<RouteDraft['fidelity']>({ mode: 'strict' });
  targets = $state<EditableTarget[]>([]);
  behavior = $state<RouteBehavior>(emptyBehavior());
  /** The route's selectors as the operator types them, in JSON. */
  selectorsText = $state('[]');
  selectors = $derived(parseSelectors(this.selectorsText));
  sync = $state(initialConcurrentEdit());
  policyDirty = $state(false);
  policyBusy = $state(false);
  hasUnsavedChanges = $derived(this.sync.dirty || this.policyDirty);
  private editVersion = 0;
  private activeResource = '';
  busy = $state('');
  publicationBlocked = $derived(
    Boolean(this.busy) || this.policyBusy || this.hasUnsavedChanges
  );
  errorMessage = $state('');
  notice = $state('');
  routingPreferences = $state('{}');
  seed = $state('setup-preview');
  simulationOperation = $state('generation');
  simulationSurface = $state('openai');
  simulationMode = $state('streaming');
  simulationDialect = $state('');
  simulationRequestJson = $state('');
  simulation = $state<RouteSimulation | null>(null);
  private simulationVersion = 0;
  simulationInputs = $derived.by(() =>
    JSON.stringify([
      this.resourceId,
      this.draft.data?.id,
      this.draft.data?.etag,
      this.simulationOperation,
      this.simulationSurface,
      this.simulationMode,
      this.simulationDialect,
      this.simulationRequestJson,
      this.seed,
      this.policyDirty,
      this.routingPreferences
    ])
  );
  activation = $state<RouteActivation | null>(null);
  editorValues = $derived({
    slug: this.slug,
    operations: this.operations,
    overallTimeoutMs: this.overallTimeoutMs,
    maxAttempts: this.maxAttempts,
    targets: this.targets,
    projectId: this.projectId,
    fidelity: this.fidelity,
    behavior: {
      ...this.behavior,
      selectors: typeof this.selectors === 'string' ? [] : this.selectors
    },
    selectorsError: typeof this.selectors === 'string' ? this.selectors : null
  });
  concurrentNotice = $derived(conflictNotice(this.sync));
  routeEligibilityWarnings = $derived(
    findRouteEligibilityWarnings(
      this.targets,
      this.modelOptions,
      this.operations
    )
  );
  routeLifecycleWarnings = $derived(
    findRouteLifecycleWarnings(this.targets, this.modelOptions)
  );

  private resetResource() {
    this.createdDraftId = '';
    this.intents.resetAll();
    this.session.advance();
    this.slug = 'default';
    this.projectId = '';
    this.operations = ['generation'];
    this.overallTimeoutMs = 120000;
    this.maxAttempts = 2;
    this.fidelity = { mode: 'strict' };
    this.targets = [];
    this.behavior = emptyBehavior();
    this.selectorsText = '[]';
    this.sync = initialConcurrentEdit();
    this.policyDirty = false;
    this.policyBusy = false;
    this.busy = '';
    this.errorMessage = '';
    this.notice = '';
    this.routingPreferences = '{}';
    this.seed = 'setup-preview';
    this.simulationOperation = 'generation';
    this.simulationSurface = 'openai';
    this.simulationMode = 'streaming';
    this.simulationDialect = '';
    this.simulationRequestJson = '';
    this.simulation = null;
    this.simulationVersion += 1;
    this.activation = null;
  }

  run = async (
    label: string,
    action: (
      isCurrent: () => boolean,
      samePrincipal: () => boolean
    ) => Promise<void>
  ) => {
    if (this.busy) return;
    const { isCurrent, samePrincipal } = this.session.capture();
    this.busy = label;
    this.errorMessage = '';
    this.notice = '';
    try {
      await action(isCurrent, samePrincipal);
    } catch (error) {
      if (!isCurrent()) return;
      if (isEtagMismatch(error)) this.sync = markConflict(this.sync);
      else this.errorMessage = errorMessage(error);
    } finally {
      if (isCurrent()) this.busy = '';
    }
  };

  invalidateSimulation = () => {
    this.simulationVersion += 1;
    this.simulation = null;
    if (this.notice === simulationNotice) this.notice = '';
  };

  touch = () => {
    this.editVersion += 1;
    this.invalidateSimulation();
    this.sync = markDirty(this.sync);
  };

  reload = async () => {
    await this.run('reload', async (isCurrent) => {
      const result = await this.draft.refetch();
      if (!isCurrent()) return;
      if (result.error) throw result.error;
      if (!result.data) throw new Error('The route draft is unavailable.');
      this.sync = beginReload(this.sync);
    });
  };

  policySaved = async (etag: string, previousEtag: string) => {
    if (this.session.isDisposed) return;
    if (this.sync.snapshotEtag === previousEtag) {
      this.sync = acceptRemote(this.sync, etag);
    }
    this.invalidateSimulation();
    await this.draft.refetch();
  };

  toggleOperation = (operation: string, checked: boolean) => {
    this.operations = checked
      ? [...new SvelteSet([...this.operations, operation])]
      : this.operations.filter((item) => item !== operation);
    this.touch();
  };

  addTarget = () => {
    const firstUnused =
      this.modelOptions.find(
        (option) =>
          !this.targets.some((target) => target.providerModelId === option.id)
      ) ?? this.modelOptions[0];
    if (!firstUnused) return;
    this.targets = [
      ...this.targets,
      {
        providerModelId: firstUnused.id,
        priority: 1,
        weight: 100,
        timeoutMs: 60000
      }
    ];
    this.touch();
  };

  removeTarget = (index: number) => {
    this.targets = this.targets.filter(
      (_, targetIndex) => targetIndex !== index
    );
    this.touch();
  };

  private readonly port: RouteDraftPort = {
    queryClient: this.queryClient,
    run: this.run,
    sync: () => this.sync,
    setSync: (next) => {
      this.sync = next;
    },
    setNotice: (message) => {
      this.notice = message;
    }
  };

  private rejectInvalidDraft() {
    const issue = validateRouteEditor(this.editorValues);
    if (issue) this.errorMessage = issue;
    return Boolean(issue);
  }

  create = async (event: SubmitEvent) => {
    event.preventDefault();
    if (!this.canManage || this.busy) return;
    if (this.createdDraftId) {
      await openCreatedDraft(this.port, this.createdDraftId);
      return;
    }
    if (this.rejectInvalidDraft()) return;
    await createDraft(this.port, {
      intent: this.createIntent,
      values: this.editorValues,
      models: this.modelOptions,
      onCreated: (id) => {
        this.createdDraftId = id;
      },
      onRefreshFailed: (message) => {
        this.errorMessage = message;
      }
    });
  };

  save = async (current: RouteDraft) => {
    if (!this.canManage || this.busy) return;
    if (!this.sync.dirty) {
      this.errorMessage = '';
      this.notice = 'No unsaved changes.';
      return;
    }
    if (this.rejectInvalidDraft()) return;
    this.invalidateSimulation();
    await saveDraft(
      this.port,
      current,
      this.editorValues,
      () => this.editVersion
    );
  };

  simulate = async (current: RouteDraft) => {
    if (!this.canManage || this.publicationBlocked) return;
    this.invalidateSimulation();
    const version = this.simulationVersion;
    const form = {
      operation: this.simulationOperation,
      surface: this.simulationSurface,
      mode: this.simulationMode,
      dialect: this.simulationDialect,
      requestJson: this.simulationRequestJson,
      seed: this.seed,
      preferencesJson: this.routingPreferences
    };
    await simulateDraft(
      this.port,
      current,
      form,
      () => version === this.simulationVersion,
      (simulation) => {
        this.simulation = simulation;
      }
    );
  };

  validate = async (current: RouteDraft) => {
    if (!this.canManage || this.publicationBlocked) return;
    await validateDraft(this.port, current);
  };

  activate = async (current: RouteDraft) => {
    if (!this.canManage || this.publicationBlocked) return;
    await activateDraft(
      this.port,
      current,
      this.activateRouteIntent,
      (activation) => {
        this.activation = activation;
      }
    );
  };

  remove = async (current: RouteDraft) => {
    if (!this.canManage || this.busy) return;
    await removeDraft(this.port, current);
  };

  constructor(readRouteId: () => string | undefined) {
    this.readRouteId = readRouteId;
    this.draft = createQuery(() => ({
      queryKey: routeKeys.draft(this.resourceId),
      queryFn: ({ queryKey, signal }) =>
        getRouteDraft(queryKey[2] as string, signal),
      enabled: Boolean(this.resourceId)
    }));
    this.providerModels = createQuery(() => ({
      queryKey: providerKeys.enabledModels(),
      queryFn: ({ signal }) => listProviderModelInventory(true, signal)
    }));
    this.toolsets = createQuery(() => ({
      queryKey: agentKeys.toolsets(),
      queryFn: () => listPublications('toolsets'),
      enabled: this.behavior.tool_execution === 'gateway'
    }));
    $effect(() => {
      const resource = this.resourceId;
      const current = this.draft.data;
      if (resource !== this.activeResource) {
        this.activeResource = resource;
        untrack(() => this.resetResource());
      }
      if (!current || current.id !== resource) return;
      const next = reconcile(this.sync, current.etag);
      if (next.state !== this.sync) this.sync = next.state;
      if (!next.hydrate) return;
      this.slug = current.slug;
      this.operations = [...current.operations];
      this.overallTimeoutMs = current.overall_timeout_ms;
      this.maxAttempts = current.max_attempts;
      this.fidelity = current.fidelity;
      this.behavior = hydrateRouteBehavior(current);
      this.selectorsText = JSON.stringify(current.selectors, null, 2);
      this.targets = hydrateRouteTargets(current);
    });
    $effect(() => {
      if (!this.operations.includes(this.simulationOperation)) {
        this.simulationOperation = this.operations[0] ?? 'generation';
      }
      const surfaces = this.surfaces;
      if (!surfaces.includes(this.simulationSurface))
        this.simulationSurface = surfaces[0] ?? 'openai';
      const modes: string[] = modesFor(this.simulationOperation);
      if (!modes.includes(this.simulationMode))
        this.simulationMode = modes[0] ?? 'unary';
      if (
        this.simulationDialect &&
        !(this.inspectionDialects as string[]).includes(this.simulationDialect)
      )
        this.simulationDialect = '';
    });
    $effect(() => {
      void this.simulationInputs;
      untrack(this.invalidateSimulation);
    });
    guardUnsavedChanges(() => this.sync.dirty);
  }
}
