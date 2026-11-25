import { createQuery } from '@tanstack/svelte-query';
import { onDestroy } from 'svelte';
import {
  routeKeys,
  listRoutes,
  selectActiveRoutes,
  simulateRouting
} from '#lib/features/routes/index.js';
import { listApiKeys, apiKeyKeys } from '#lib/features/access/index.js';
import { useAction } from '#lib/editing/action.svelte.js';
import { captureResult } from '#lib/editing/result.js';
import { nativeObject, parseNativeJSON } from '#lib/nativeJson.js';
import { abortError, errorMessage } from '#lib/management/http.js';
import {
  runPlayground,
  streamPlayground,
  type PlaygroundOperation,
  type PlaygroundRequest,
  type PlaygroundResponse,
  type PlaygroundStreamDone
} from './api';
import { PlaygroundInspection } from './playgroundInspection.svelte';
import { resolvePlaygroundPlan, type Composer } from './playgroundPlan';
import { playgroundTemplates, templateFor } from './templates';
import {
  parseMaxOutputTokens,
  parseResponseSchema,
  parseTemperature,
  parseTools
} from './validation';

type Mode = 'text' | 'tools' | 'structured';

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
  surface = $state<'openai' | 'anthropic' | 'gemini'>('openai');
  model = $state('');
  input = $state('');
  rawJson = $state(JSON.stringify(playgroundTemplates[0].request, null, 2));
  templateKey = $state(playgroundTemplates[0].key);
  nativeDialect = $state('');
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
  // Bumped whenever a streaming check is started or invalidated, so a slower,
  // older check cannot overwrite the verdict for the current inputs.
  private streamCheckSerial = 0;
  temperature = $state('');
  maxOutputTokens = $state('');
  toolsJson = $state(
    '[\n  {\n    "name": "get_weather",\n    "description": "Get weather for a city",\n    "input_schema": {\n      "type": "object",\n      "properties": { "city": { "type": "string" } },\n      "required": ["city"]\n    }\n  }\n]'
  );
  schemaJson = $state(
    '{\n  "type": "object",\n  "properties": {\n    "answer": { "type": "string" }\n  },\n  "required": ["answer"],\n  "additionalProperties": false\n}'
  );
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
  readonly execution = useAction();
  response = $state<PlaygroundResponse | null>(null);
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
  inspectDialect = $state('');
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
  streamSelectable = $derived(this.activeOperation === 'generation');
  readonly inspection = new PlaygroundInspection(this);
  // A dry run is the most recent action when its inputs still match; a run resets it.
  explanation = $derived(
    this.inspection.current
      ? { dryRun: true, decisions: this.inspection.current }
      : this.streamDone?.routing?.length
        ? { dryRun: false, decisions: this.streamDone.routing }
        : this.response?.routing?.length
          ? { dryRun: false, decisions: this.response.routing }
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

  private async checkStreamCapability() {
    const serial = ++this.streamCheckSerial;
    this.streamCheckMessage = '';
    if (!this.selectedRoute) {
      this.streamCheck = 'unknown';
      this.streamCheckMessage =
        'The route slug is not an active route, so streaming support cannot be verified.';
      return;
    }
    this.streamCheck = 'checking';
    try {
      const decisions = await simulateRouting({
        route: this.model.trim(),
        surface: this.surface,
        mode: 'streaming',
        preferences: JSON.parse(this.routing || '{}')
      });
      if (serial !== this.streamCheckSerial) return;
      if (decisions.some((decision) => decision.eligible)) {
        this.streamCheck = 'ok';
      } else {
        this.streamCheck = 'unsupported';
        this.streamCheckMessage =
          'No published target reports streaming eligibility for this route and surface.';
      }
    } catch {
      if (serial !== this.streamCheckSerial) return;
      this.streamCheck = 'unknown';
      this.streamCheckMessage =
        'Streaming capability could not be verified; the route may reject the stream.';
    }
  }

  toggleStream = (enabled: boolean) => {
    this.streamEnabled = enabled;
    if (enabled) void this.checkStreamCapability();
    else {
      this.streamCheckSerial++;
      this.streamCheck = 'idle';
      this.streamCheckMessage = '';
    }
  };

  cancelStream = () => {
    this.streamAbort?.abort();
  };

  clearResult = () => {
    this.streamAbort?.abort();
    this.streamFrames = [];
    this.streamDone = null;
    this.streamProblem = null;
    this.execution.retire();
    this.response = null;
    this.completedRequest = null;
  };

  advancedRequest(): Record<string, unknown> {
    if (this.operation === 'translation') return { model: this.model.trim() };
    const raw = parseNativeJSON(this.rawJson);
    if (!nativeObject(raw))
      throw new Error('The request document must be a JSON object.');
    return raw;
  }

  private requestControls() {
    return {
      temperature: parseTemperature(this.temperature),
      max_output_tokens: parseMaxOutputTokens(this.maxOutputTokens),
      tools: this.mode === 'tools' ? parseTools(this.toolsJson) : undefined,
      response_format:
        this.mode === 'structured'
          ? parseResponseSchema(this.schemaJson)
          : undefined
    };
  }

  private async runStream(request: PlaygroundRequest) {
    const abort = new AbortController();
    this.streamAbort = abort;
    this.execution.retire();
    this.response = null;
    this.streaming = true;
    this.streamFrames = [];
    this.streamDone = null;
    this.streamProblem = null;
    this.completedRequest = request;
    let terminal = false;
    try {
      await streamPlayground(
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
      );
      // A clean close without done or error means the response was cut short.
      if (!terminal && !abort.signal.aborted)
        this.streamProblem = 'The stream ended before completion.';
    } catch (error) {
      if (!abortError(error))
        this.streamProblem = errorMessage(
          error,
          'The playground stream failed.'
        );
    } finally {
      this.streaming = false;
      this.streamAbort = null;
    }
  }

  private composedRequest(advanced: boolean): PlaygroundRequest {
    return advanced
      ? {
          routing: JSON.parse(this.routing),
          model: this.model.trim(),
          surface: this.surface,
          operation: this.activeOperation as PlaygroundRequest['operation'],
          request: this.advancedRequest(),
          stream: this.streamEnabled ? true : undefined
        }
      : {
          routing: JSON.parse(this.routing),
          model: this.model.trim(),
          input: this.input,
          surface: this.surface,
          stream: this.streamEnabled ? true : undefined,
          ...this.requestControls()
        };
  }

  submit = async (event: SubmitEvent) => {
    event.preventDefault();
    this.validationError = '';
    // The run that follows produces its own explanation, so the dry run stops
    // competing for the panel.
    this.inspection.reset();
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
    const advanced = plan.kind === 'composer' && plan.advanced;
    if (!advanced && !this.input.trim()) {
      this.validationError = 'Enter a prompt.';
      return;
    }
    const built = captureResult(() => this.composedRequest(advanced));
    if (!built.ok) {
      this.validationError = errorMessage(
        built.error,
        'Check the request fields.'
      );
      return;
    }
    const request = built.value;
    this.streamFrames = [];
    this.streamDone = null;
    this.streamProblem = null;
    this.execution.retire();
    this.response = null;
    this.completedRequest = request;
    if (this.streamEnabled) {
      await this.runStream(request);
      return;
    }
    await this.execution.run(
      () => runPlayground(request),
      (response) => {
        this.response = response;
      },
      { fallback: 'The playground request failed.' }
    );
  };

  constructor() {
    $effect(() => {
      void this.model;
      void this.surface;
      void this.activeOperation;
      this.streamCheckSerial++;
      this.streamEnabled = false;
      this.streamCheck = 'idle';
      this.streamCheckMessage = '';
    });

    $effect(() => {
      if (
        this.activeOperation !== 'generation' &&
        this.activeOperation !== 'token_count' &&
        this.surface !== 'openai'
      )
        this.surface = 'openai';
    });

    onDestroy(() => {
      this.streamAbort?.abort();
    });
  }
}
