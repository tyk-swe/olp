import { providerKeys } from '$lib/features/providers/providerKeys';
import { routeKeys } from '$lib/features/routes/routeKeys';
import { untrack } from 'svelte';
import { SvelteSet } from 'svelte/reactivity';
import { goto } from '$app/navigation';
import { guardUnsavedChanges } from '$lib/forms/unsavedChanges';
import { resolve } from '$app/paths';
import { createQuery, useQueryClient } from '@tanstack/svelte-query';
import { errorMessage as message, isEtagMismatch } from '$lib/api/http';
import {
  beginReload,
  conflictNotice,
  initialConcurrentEdit,
  markConflict,
  markDirty,
  markSaved,
  reconcile,
  acceptRemote
} from '$lib/forms/concurrentEdit';
import {
  activateRoute,
  createRouteDraft,
  deleteRouteDraft,
  getRouteDraft,
  replaceRouteDraft,
  simulateRoute,
  validateRoute,
  type RouteActivation,
  type RouteDraft,
  type RouteSimulation
} from '$lib/features/routes/api';
import { listProviderModelInventory } from '$lib/features/providers/models';
import { useRole } from '$lib/features/access/session/useRole.svelte';
import {
  buildCreateRouteDraftInput,
  buildReplaceRouteDraftInput,
  modesFor,
  routeEligibilityWarnings as findRouteEligibilityWarnings,
  surfacesFor,
  toRouteModelOptions,
  validateRouteEditor,
  type EditableTarget
} from '$lib/features/routes/routeEditor';

const simulationNotice =
  'Deterministic attempt order calculated from the saved draft.';

export class RouteDraftEditorState {
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
  modelOptions = $derived.by(() =>
    toRouteModelOptions(this.providerModels.data ?? [])
  );
  slug = $state('default');
  operations = $state<string[]>(['generation']);
  overallTimeoutMs = $state(120000);
  maxAttempts = $state(2);
  targets = $state<EditableTarget[]>([]);
  sync = $state(initialConcurrentEdit());
  private editVersion = 0;
  busy = $state('');
  errorMessage = $state('');
  notice = $state('');
  seed = $state('setup-preview');
  simulationOperation = $state('generation');
  simulationSurface = $state('openai');
  simulationMode = $state('streaming');
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
      this.seed
    ])
  );
  activation = $state<RouteActivation | null>(null);
  editorValues = $derived({
    slug: this.slug,
    operations: this.operations,
    overallTimeoutMs: this.overallTimeoutMs,
    maxAttempts: this.maxAttempts,
    targets: this.targets
  });
  concurrentNotice = $derived(conflictNotice(this.sync));
  routeEligibilityWarnings = $derived(
    findRouteEligibilityWarnings(
      this.targets,
      this.modelOptions,
      this.operations
    )
  );
  run = async (label: string, action: () => Promise<void>) => {
    if (this.busy) return;
    this.busy = label;
    this.errorMessage = '';
    this.notice = '';
    try {
      await action();
    } catch (error) {
      if (isEtagMismatch(error)) this.sync = markConflict(this.sync);
      else this.errorMessage = message(error);
    } finally {
      this.busy = '';
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
    await this.run('reload', async () => {
      const result = await this.draft.refetch();
      if (result.error) throw result.error;
      if (!result.data) throw new Error('The route draft is unavailable.');
      this.sync = beginReload(this.sync);
    });
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
  create = async (event: SubmitEvent) => {
    event.preventDefault();
    if (!this.canManage || this.busy) return;
    const issue = validateRouteEditor(this.editorValues);
    if (issue) {
      this.errorMessage = issue;
      return;
    }
    await this.run('save', async () => {
      const id = await createRouteDraft(
        buildCreateRouteDraftInput(this.editorValues, this.modelOptions)
      );
      await this.queryClient.invalidateQueries({
        queryKey: routeKeys.lists
      });
      this.sync = initialConcurrentEdit();
      await goto(resolve(`/routes/${id}`));
    });
  };
  save = async (current: RouteDraft) => {
    if (!this.canManage || this.busy) return;
    const issue = validateRouteEditor(this.editorValues);
    if (issue) {
      this.errorMessage = issue;
      return;
    }
    this.invalidateSimulation();
    await this.run('save', async () => {
      if (!this.sync.snapshotEtag)
        throw new Error('Reload the draft before saving.');
      const submittedVersion = this.editVersion;
      const updated = await replaceRouteDraft(
        current.id,
        this.sync.snapshotEtag,
        buildReplaceRouteDraftInput(this.editorValues)
      );
      this.sync = markSaved(
        this.sync,
        updated.etag,
        this.editVersion !== submittedVersion
      );
      this.queryClient.setQueryData(routeKeys.draft(current.id), updated);
      this.notice = this.sync.dirty
        ? 'Draft saved. You have additional unsaved changes.'
        : 'Draft saved. Validate to preview, or activate directly; activation validates the saved draft.';
    });
  };
  simulate = async (current: RouteDraft) => {
    if (!this.canManage || this.busy || this.sync.dirty) return;
    this.invalidateSimulation();
    const version = this.simulationVersion;
    await this.run('simulate', async () => {
      let simulation: RouteSimulation;
      try {
        simulation = await simulateRoute(current.id, {
          operation: this.simulationOperation,
          surface: this.simulationSurface,
          mode: this.simulationMode,
          seed: this.seed || 'preview'
        });
      } catch (error) {
        if (version === this.simulationVersion) throw error;
        return;
      }
      if (version !== this.simulationVersion) return;
      this.simulation = simulation;
      this.notice = simulationNotice;
    });
  };
  validate = async (current: RouteDraft) => {
    if (!this.canManage || this.busy || this.sync.dirty) return;
    await this.run('validate', async () => {
      const validation = await validateRoute(current);
      this.sync = acceptRemote(this.sync, validation.etag);
      this.queryClient.setQueryData<RouteDraft>(routeKeys.draft(current.id), {
        ...current,
        state: validation.state,
        etag: validation.etag
      });
      this.notice = 'Validation passed. The saved draft is ready to activate.';
    });
  };
  activate = async (current: RouteDraft) => {
    if (!this.canManage || this.busy || this.sync.dirty) return;
    await this.run('activate', async () => {
      this.activation = await activateRoute(current);
      // Activation returns the draft to `draft` under a fresh ETag. Adopting it
      // here keeps the next save from failing its If-Match precondition.
      this.sync = acceptRemote(this.sync, this.activation.draft_etag);
      this.queryClient.setQueryData<RouteDraft>(routeKeys.draft(current.id), {
        ...current,
        state: 'draft',
        etag: this.activation.draft_etag
      });
      this.notice = `Route activated as revision ${this.activation.revision} in runtime generation ${this.activation.runtime_generation.sequence}.`;
      await Promise.all([
        this.draft.refetch(),
        this.queryClient.invalidateQueries({ queryKey: routeKeys.lists })
      ]);
    });
  };
  remove = async (current: RouteDraft) => {
    if (!this.canManage || this.busy) return;
    if (!confirm(`Delete draft “${current.slug}”?`)) return;
    await this.run('delete', async () => {
      await deleteRouteDraft(current.id, current.etag);
      await this.queryClient.invalidateQueries({
        queryKey: routeKeys.lists
      });
      this.sync = initialConcurrentEdit();
      await goto(resolve('/routes'));
    });
  };
  constructor(readRouteId: () => string | undefined) {
    this.readRouteId = readRouteId;
    this.draft = createQuery(() => ({
      queryKey: routeKeys.draft(this.resourceId),
      queryFn: () => getRouteDraft(this.resourceId),
      enabled: Boolean(this.resourceId)
    }));
    this.providerModels = createQuery(() => ({
      queryKey: providerKeys.enabledModels(),
      queryFn: () => listProviderModelInventory(true)
    }));
    $effect(() => {
      const current = this.draft.data;
      if (!current) return;
      const next = reconcile(this.sync, current.etag);
      if (next.state !== this.sync) this.sync = next.state;
      if (!next.hydrate) return;
      this.slug = current.slug;
      this.operations = [...current.operations];
      this.overallTimeoutMs = current.overall_timeout_ms;
      this.maxAttempts = current.max_attempts;
      this.targets = current.targets.map((target) => ({
        providerModelId: target.provider_model_id,
        priority: target.priority,
        weight: target.weight,
        timeoutMs: target.timeout_ms
      }));
    });
    $effect(() => {
      if (!this.operations.includes(this.simulationOperation)) {
        this.simulationOperation = this.operations[0] ?? 'generation';
      }
      const surfaces = surfacesFor(this.simulationOperation);
      if (!surfaces.includes(this.simulationSurface))
        this.simulationSurface = surfaces[0] ?? 'openai';
      const modes = modesFor(this.simulationOperation);
      if (!modes.includes(this.simulationMode))
        this.simulationMode = modes[0] ?? 'unary';
    });
    $effect(() => {
      void this.simulationInputs;
      untrack(this.invalidateSimulation);
    });
    guardUnsavedChanges(() => this.sync.dirty);
  }
}
