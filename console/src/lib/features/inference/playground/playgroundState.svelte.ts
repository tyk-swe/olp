import { createMutation, createQuery } from '@tanstack/svelte-query';
import { onDestroy } from 'svelte';
import { readResult } from '#lib/editing/result.js';
import { apiKeyKeys, listApiKeys } from '#lib/features/access/index.js';
import {
  listRoutes,
  routeKeys,
  selectActiveRoutes,
  simulateRouting
} from '#lib/features/routes/index.js';
import { abortError, errorMessage } from '#lib/management/http.js';
import {
  inspectRouting,
  runPlayground,
  streamPlayground,
  type InspectRoutingInput,
  type PlaygroundOperation,
  type PlaygroundRequest,
  type PlaygroundStreamDone
} from './api';
import {
  resolvePlaygroundPlan,
  type Composer,
  type Mode,
  type PlaygroundSurface
} from './playgroundPlan';
import { playgroundTemplates, templateFor } from './templates';
import {
  buildInspectionInput,
  buildPlaygroundRequest,
  parseRoutingPreferences
} from './validation';

const defaultToolsJson =
  '[\n  {\n    "name": "get_weather",\n    "description": "Get weather for a city",\n    "input_schema": {\n      "type": "object",\n      "properties": { "city": { "type": "string" } },\n      "required": ["city"]\n    }\n  }\n]';

const defaultSchemaJson =
  '{\n  "type": "object",\n  "properties": {\n    "answer": { "type": "string" }\n  },\n  "required": ["answer"],\n  "additionalProperties": false\n}';

const streamVerifyFailed =
  'Streaming capability could not be verified; the route may reject the stream.';

export class PlaygroundState {
  private routingPreferences = $state('{}');
  get routing() {
    return this.routingPreferences;
  }
  set routing(value: string) {
    if (value === this.routingPreferences) return;
    this.routingPreferences = value;
    if (this.streamEnabled) void this.checkStreamCapability();
  }
  mode = $state<Mode>('text');
  composer = $state<Composer>('basic');
  operation = $state<PlaygroundOperation>('generation');
  surface = $state<PlaygroundSurface>('openai');
  model = $state('');
  input = $state('');
  rawJson = $state(JSON.stringify(playgroundTemplates[0].request, null, 2));
  templateKey = $state(playgroundTemplates[0].key);
  nativeDialect = $state('');
  inspectDialect = $state('');
  streamEnabled = $state(false);
  streamCheck = $state<'idle' | 'checking' | 'ok' | 'unsupported' | 'unknown'>(
    'idle'
  );
  streamCheckMessage = $state('');
  streaming = $state(false);
  streamFrames = $state<string[]>([]);
  streamDone = $state<PlaygroundStreamDone | null>(null);
  streamProblem = $state<string | null>(null);
  private streamAbort: AbortController | null = null;
  private streamCheckSerial = 0;
  temperature = $state('');
  maxOutputTokens = $state('');
  toolsJson = $state(defaultToolsJson);
  schemaJson = $state(defaultSchemaJson);
  validationError = $state('');
  completedRequest = $state<PlaygroundRequest | null>(null);
  routes = createQuery(() => ({
    queryKey: routeKeys.all(),
    queryFn: ({ signal }) => listRoutes(signal),
    select: selectActiveRoutes
  }));
  apiKeys = createQuery(() => ({
    queryKey: apiKeyKeys.list(),
    queryFn: ({ signal }) => listApiKeys(signal)
  }));
  mutation = createMutation(() => ({ mutationFn: runPlayground }));
  selectedRoute = $derived(
    (this.routes.data ?? []).find((route) => route.slug === this.model.trim())
  );
  strictSelected = $derived(
    this.selectedRoute?.latest_revision?.fidelity?.mode === 'strict'
  );
  routeOperations = $derived(
    this.selectedRoute?.latest_revision?.operations ?? null
  );
  activeOperation = $derived<PlaygroundOperation>(
    this.composer === 'advanced' ? this.operation : 'generation'
  );
  operationKnown = $derived(
    this.routeOperations == null ||
      this.routeOperations.includes(this.activeOperation)
  );
  plan = $derived(
    resolvePlaygroundPlan({
      composer: this.composer,
      operation: this.operation,
      surface: this.surface,
      strict: this.strictSelected,
      template: this.templateKey,
      nativeDialect: this.nativeDialect,
      inspectDialect: this.inspectDialect,
      stream: this.streamEnabled,
      operationKnown: this.operationKnown
    })
  );
  simulation = createMutation(() => ({
    mutationFn: (input: InspectRoutingInput) => inspectRouting(input)
  }));
  simulateKeyId = $state('');
  simulateSeed = $state('');
  private inspectedInputs = $state('');
  simulationError = $state('');
  explanation = $derived(
    this.simulation.data?.length &&
      this.inspectedInputs === this.inspectionInputs()
      ? { dryRun: true, decisions: this.simulation.data }
      : this.streamDone?.routing?.length
        ? { dryRun: false, decisions: this.streamDone.routing }
        : this.mutation.data?.routing?.length
          ? { dryRun: false, decisions: this.mutation.data.routing }
          : null
  );

  applyTemplate = (key: string) => {
    this.templateKey = key;
    const template = templateFor(key);
    if (!template) return;
    this.operation = template.operation;
    if (template.surface) this.surface = template.surface;
    this.nativeDialect = template.nativeDialect ?? '';
    this.inspectDialect = template.inspectDialect ?? '';
    this.rawJson = JSON.stringify(template.request, null, 2);
  };

  private resetStreamCheck() {
    this.streamCheckSerial++;
    this.streamCheck = 'idle';
    this.streamCheckMessage = '';
  }

  private resetOutput() {
    this.streamFrames = [];
    this.streamDone = null;
    this.streamProblem = null;
    this.mutation.reset();
  }

  private setStreamStatus(status: 'unknown' | 'unsupported', message: string) {
    this.streamCheck = status;
    this.streamCheckMessage = message;
  }

  private async checkStreamCapability() {
    const serial = ++this.streamCheckSerial;
    this.streamCheckMessage = '';
    if (!this.selectedRoute) {
      this.setStreamStatus(
        'unknown',
        'The route slug is not an active route, so streaming support cannot be verified.'
      );
      return;
    }
    const preferences = parseRoutingPreferences(this.routing || '{}');
    if (!preferences.ok) {
      this.setStreamStatus('unknown', streamVerifyFailed);
      return;
    }
    this.streamCheck = 'checking';
    const result = await readResult(() =>
      simulateRouting({
        route: this.model.trim(),
        surface: this.surface,
        mode: 'streaming',
        preferences: preferences.value
      })
    );
    if (serial !== this.streamCheckSerial) return;
    if (!result.ok) {
      this.setStreamStatus('unknown', streamVerifyFailed);
      return;
    }
    if (result.value.some((decision) => decision.eligible)) {
      this.streamCheck = 'ok';
      return;
    }
    this.setStreamStatus(
      'unsupported',
      'No published target reports streaming eligibility for this route and surface.'
    );
  }

  toggleStream = (enabled: boolean) => {
    this.streamEnabled = enabled;
    if (enabled) void this.checkStreamCapability();
    else this.resetStreamCheck();
  };

  cancelStream = () => {
    this.streamAbort?.abort();
  };

  clearResult = () => {
    this.streamAbort?.abort();
    this.resetOutput();
    this.completedRequest = null;
  };

  private inspectionInputs() {
    return JSON.stringify([
      this.model,
      this.surface,
      this.operation,
      this.composer,
      this.rawJson,
      this.streamEnabled,
      this.routing,
      this.simulateKeyId,
      this.simulateSeed,
      this.inspectDialect,
      this.nativeDialect
    ]);
  }

  explain = async () => {
    this.simulationError = '';
    if (!this.model.trim()) {
      this.simulationError = 'Enter an active route slug.';
      return;
    }
    const version = this.inspectionInputs();
    const input = buildInspectionInput({
      composer: this.composer,
      model: this.model,
      operation: this.activeOperation,
      rawJson: this.rawJson,
      routing: this.routing,
      stream: this.streamEnabled,
      simulateKeyId: this.simulateKeyId,
      simulateSeed: this.simulateSeed,
      plan: this.plan
    });
    if (!input.ok) {
      this.simulationError = input.error;
      return;
    }
    const result = await readResult(() =>
      this.simulation.mutateAsync(input.value)
    );
    if (!result.ok) {
      this.simulationError = errorMessage(
        result.error,
        'The routing explanation could not be produced.'
      );
      return;
    }
    this.inspectedInputs = version;
  };

  private async runStream(request: PlaygroundRequest) {
    const abort = new AbortController();
    this.streamAbort = abort;
    this.resetOutput();
    this.streaming = true;
    this.completedRequest = request;
    let terminal = false;
    const result = await readResult(() =>
      streamPlayground(
        request,
        {
          frame: (frame) => {
            this.streamFrames = [...this.streamFrames, frame];
          },
          done: (meta) => {
            terminal = true;
            this.streamDone = meta;
          },
          error: (problem) => {
            terminal = true;
            this.streamProblem =
              problem.message ?? 'The playground stream failed.';
          }
        },
        abort.signal
      )
    );
    if (!result.ok) {
      if (!abortError(result.error)) {
        this.streamProblem = errorMessage(
          result.error,
          'The playground stream failed.'
        );
      }
    } else if (!terminal && !abort.signal.aborted) {
      this.streamProblem = 'The stream ended before completion.';
    }
    this.streaming = false;
    this.streamAbort = null;
  }

  submit = async (event: SubmitEvent) => {
    event.preventDefault();
    this.validationError = '';
    this.simulation.reset();
    this.simulationError = '';
    if (!this.model.trim()) {
      this.validationError = 'Enter an active route slug.';
      return;
    }
    const plan = this.plan;
    if (plan.submissionError) {
      this.validationError = plan.submissionError;
      return;
    }
    if (this.streamEnabled && this.streamCheck !== 'ok') {
      this.validationError =
        'Streaming has not been verified as available for this route.';
      return;
    }
    const built = buildPlaygroundRequest({
      advanced: plan.kind === 'composer' && plan.advanced,
      model: this.model,
      surface: this.surface,
      operation: this.activeOperation,
      routing: this.routing,
      rawJson: this.rawJson,
      input: this.input,
      mode: this.mode,
      temperature: this.temperature,
      maxOutputTokens: this.maxOutputTokens,
      toolsJson: this.toolsJson,
      schemaJson: this.schemaJson,
      stream: this.streamEnabled
    });
    if (!built.ok) {
      this.validationError = built.error;
      return;
    }
    this.resetOutput();
    this.completedRequest = built.value;
    if (this.streamEnabled) {
      await this.runStream(built.value);
      return;
    }
    const result = await readResult(() =>
      this.mutation.mutateAsync(built.value)
    );
    if (!result.ok) this.validationError = '';
  };

  constructor() {
    $effect(() => {
      void this.model;
      void this.surface;
      void this.activeOperation;
      this.streamEnabled = false;
      this.resetStreamCheck();
    });

    $effect(() => {
      if (
        this.activeOperation !== 'generation' &&
        this.activeOperation !== 'token_count' &&
        this.surface !== 'openai'
      ) {
        this.surface = 'openai';
      }
    });

    onDestroy(() => {
      this.streamAbort?.abort();
    });
  }
}
