import { overviewKeys } from '$lib/features/overview/overviewKeys';
import { providerKeys } from '$lib/features/providers/providerKeys';
import { routeKeys } from '$lib/features/routes/routeKeys';
import { onDestroy, untrack } from 'svelte';
import { SvelteSet } from 'svelte/reactivity';
import { goto } from '$app/navigation';
import { guardUnsavedChanges } from '$lib/forms/unsavedChanges';
import { nativeObject, parseNativeJSON } from '$lib/json/nativeJson';
import type { components } from '$lib/api/schema';
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
  hasOutputRules,
  modesFor,
  policyRulesFrom,
  routeEligibilityWarnings as findRouteEligibilityWarnings,
  surfacesFor,
  toRouteModelOptions,
  validateRouteEditor,
  type EditablePolicyRule,
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
  projectId = $state('');
  operations = $state<string[]>(['generation']);
  overallTimeoutMs = $state(120000);
  maxAttempts = $state(2);
  fidelity = $state<RouteDraft['fidelity']>({ mode: 'strict' });
  targets = $state<EditableTarget[]>([]);
  policyRules = $state<EditablePolicyRule[]>([]);
  outputPolicyActive = $derived(hasOutputRules(this.policyRules));
  sync = $state(initialConcurrentEdit());
  policyDirty = $state(false);
  policyBusy = $state(false);
  hasUnsavedChanges = $derived(this.sync.dirty || this.policyDirty);
  private editVersion = 0;
  /**
   * Ownership generation for asynchronous actions. It advances whenever the
   * editor's resource identity changes, so a completion from another draft's
   * lifetime is dropped instead of being applied to the resource on screen.
   */
  private ownerEpoch = 0;
  private disposed = false;
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
    contentPolicyRules: this.policyRules,
    projectId: this.projectId,
    fidelity: this.fidelity
  });
  concurrentNotice = $derived(conflictNotice(this.sync));
  routeEligibilityWarnings = $derived(
    findRouteEligibilityWarnings(
      this.targets,
      this.modelOptions,
      this.operations
    )
  );
  /**
   * True while the captured epoch still owns this editor: the component is
   * mounted and the resource identity has not moved on.
   */
  private current(epoch: number): boolean {
    return !this.disposed && epoch === this.ownerEpoch;
  }
  /**
   * Returns every form field, flag, and result to its initial value and
   * invalidates every in-flight action captured under the previous resource.
   */
  private resetResource() {
    this.ownerEpoch += 1;
    this.slug = 'default';
    this.projectId = '';
    this.operations = ['generation'];
    this.overallTimeoutMs = 120000;
    this.maxAttempts = 2;
    this.fidelity = { mode: 'strict' };
    this.targets = [];
    this.policyRules = [];
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
    action: (isCurrent: () => boolean) => Promise<void>
  ) => {
    if (this.busy) return;
    const epoch = this.ownerEpoch;
    const isCurrent = () => this.current(epoch);
    this.busy = label;
    this.errorMessage = '';
    this.notice = '';
    try {
      await action(isCurrent);
    } catch (error) {
      if (!isCurrent()) return;
      if (isEtagMismatch(error)) this.sync = markConflict(this.sync);
      else this.errorMessage = message(error);
    } finally {
      // A stale action must not release a newer operation's busy state.
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
    // The routing-policy child only invokes this while it still owns its
    // resource, but keep the disposal guard so a late callback is inert.
    if (this.disposed) return;
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
  addPolicyRule = () => {
    this.policyRules = [
      ...this.policyRules,
      {
        id: `rule-${this.policyRules.length + 1}`,
        phase: 'input',
        action: this.fidelity.mode === 'strict' ? 'block' : 'redact',
        pattern: '',
        replacement: ''
      }
    ];
    this.touch();
  };
  removePolicyRule = (index: number) => {
    this.policyRules = this.policyRules.filter(
      (_, ruleIndex) => ruleIndex !== index
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
    await this.run('save', async (isCurrent) => {
      const { id } = await createRouteDraft(
        buildCreateRouteDraftInput(this.editorValues, this.modelOptions)
      );
      // The create already committed, so the list invalidation stays valid
      // even if this editor has since been replaced or destroyed.
      await this.queryClient.invalidateQueries({
        queryKey: routeKeys.lists
      });
      if (!isCurrent()) return;
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
    await this.run('save', async (isCurrent) => {
      if (!this.sync.snapshotEtag)
        throw new Error('Reload the draft before saving.');
      const submittedVersion = this.editVersion;
      const updated = await replaceRouteDraft(
        current.id,
        this.sync.snapshotEtag,
        buildReplaceRouteDraftInput(this.editorValues)
      );
      // The response is stored under the originating draft's key, which is
      // correct regardless of which resource the editor now shows.
      this.queryClient.setQueryData(routeKeys.draft(current.id), updated);
      if (!isCurrent()) return;
      this.sync = markSaved(
        updated.etag,
        this.editVersion !== submittedVersion
      );
      this.notice = this.sync.dirty
        ? 'Draft saved. You have additional unsaved changes.'
        : 'Draft saved. Validate to preview, or activate directly; activation validates the saved draft.';
    });
  };
  simulate = async (current: RouteDraft) => {
    if (!this.canManage || this.publicationBlocked) return;
    this.invalidateSimulation();
    const version = this.simulationVersion;
    await this.run('simulate', async (isCurrent) => {
      let simulation: RouteSimulation;
      try {
        const source = this.simulationRequestJson.trim();
        let nativeRequest: Record<string, unknown> | undefined;
        if (source) {
          const parsed = parseNativeJSON(source);
          if (!nativeObject(parsed))
            throw new Error('The native request must be a JSON object.');
          nativeRequest = parsed;
        }
        simulation = await simulateRoute(current.id, {
          operation: this.simulationOperation,
          surface: this.simulationSurface,
          mode: this.simulationMode,
          seed: this.seed || 'preview',
          preferences: JSON.parse(this.routingPreferences),
          request: nativeRequest,
          dialect: (this.simulationDialect || undefined) as
            components['schemas']['SimulationDialect'] | undefined
        });
      } catch (error) {
        if (isCurrent() && version === this.simulationVersion) throw error;
        return;
      }
      if (!isCurrent() || version !== this.simulationVersion) return;
      this.simulation = simulation;
      this.notice = simulationNotice;
    });
  };
  validate = async (current: RouteDraft) => {
    if (!this.canManage || this.publicationBlocked) return;
    await this.run('validate', async (isCurrent) => {
      const validation = await validateRoute(current);
      this.queryClient.setQueryData<RouteDraft>(routeKeys.draft(current.id), {
        ...current,
        state: validation.state,
        etag: validation.etag
      });
      if (!isCurrent()) return;
      this.sync = acceptRemote(this.sync, validation.etag);
      this.notice = 'Validation passed. The saved draft is ready to activate.';
    });
  };
  activate = async (current: RouteDraft) => {
    if (!this.canManage || this.publicationBlocked) return;
    await this.run('activate', async (isCurrent) => {
      const activation = await activateRoute(current);
      // Adopt our activation's ETag before cache reconciliation can treat it
      // as a remote change to edits made while activation was in flight.
      if (isCurrent()) {
        this.sync = acceptRemote(this.sync, activation.draft_etag);
      }
      this.queryClient.setQueryData<RouteDraft>(routeKeys.draft(current.id), {
        ...current,
        state: 'draft',
        etag: activation.draft_etag
      });
      await Promise.all([
        this.queryClient.invalidateQueries({
          queryKey: routeKeys.draft(current.id)
        }),
        this.queryClient.invalidateQueries({ queryKey: routeKeys.lists }),
        this.queryClient.invalidateQueries({ queryKey: overviewKeys.root })
      ]);
      if (!isCurrent()) return;
      this.activation = activation;
      this.notice = `Route activated as revision ${activation.revision} in runtime generation ${activation.runtime_generation.sequence}.`;
    });
  };
  remove = async (current: RouteDraft) => {
    if (!this.canManage || this.busy) return;
    if (!confirm(`Delete draft “${current.slug}”?`)) return;
    await this.run('delete', async (isCurrent) => {
      await deleteRouteDraft(current.id, current.etag);
      await this.queryClient.invalidateQueries({
        queryKey: routeKeys.lists
      });
      if (!isCurrent()) return;
      this.sync = initialConcurrentEdit();
      await goto(resolve('/routes'));
    });
  };
  constructor(readRouteId: () => string | undefined) {
    this.readRouteId = readRouteId;
    this.draft = createQuery(() => ({
      queryKey: routeKeys.draft(this.resourceId),
      // Bind the fetch to the resolved key so a resource change cannot write
      // one draft's payload under another draft's cache entry, and so a
      // superseded observer's request is actually aborted.
      queryFn: ({ queryKey, signal }) =>
        getRouteDraft(queryKey[2] as string, signal),
      enabled: Boolean(this.resourceId)
    }));
    this.providerModels = createQuery(() => ({
      queryKey: providerKeys.enabledModels(),
      queryFn: ({ signal }) => listProviderModelInventory(true, signal)
    }));
    onDestroy(() => {
      this.disposed = true;
      this.ownerEpoch += 1;
    });
    $effect(() => {
      const resource = this.resourceId;
      const current = this.draft.data;
      if (resource !== this.activeResource) {
        // The editor moved to a different draft: nothing from the previous
        // resource may carry over, and any action it still has in flight loses
        // ownership here rather than at some later completion.
        this.activeResource = resource;
        untrack(() => this.resetResource());
      }
      // Only hydrate from a payload that belongs to the resource this editor
      // currently owns; a lagging observer can briefly hold the previous
      // draft's data while the key change propagates.
      if (!current || current.id !== resource) return;
      const next = reconcile(this.sync, current.etag);
      if (next.state !== this.sync) this.sync = next.state;
      if (!next.hydrate) return;
      this.slug = current.slug;
      this.operations = [...current.operations];
      this.overallTimeoutMs = current.overall_timeout_ms;
      this.maxAttempts = current.max_attempts;
      this.fidelity = current.fidelity;
      this.policyRules = policyRulesFrom(current.content_policy);
      this.targets = current.targets.map((target) => ({
        providerModelId: target.provider_model_id,
        priority: target.priority,
        weight: target.weight,
        timeoutMs: target.timeout_ms,
        stored: {
          providerModelId: target.provider_model_id,
          available: target.available,
          providerName: target.provider_name,
          providerModel: target.provider_model
        }
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
      if (this.outputPolicyActive && this.simulationMode === 'streaming')
        this.simulationMode = 'unary';
    });
    $effect(() => {
      void this.simulationInputs;
      untrack(this.invalidateSimulation);
    });
    guardUnsavedChanges(() => this.sync.dirty);
  }
}
