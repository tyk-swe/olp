import { createMutation, createQuery } from '@tanstack/svelte-query';
import { onDestroy } from 'svelte';
import { routeKeys } from '$lib/features/routes/routeKeys';
import { listRoutes, simulateRouting } from '$lib/features/routes/api';
import { hasOutputRules } from '$lib/features/routes/routeEditor';
import { inspectionDialects } from '$lib/features/routes/inspectionDialects';
import { listApiKeys } from '$lib/features/access/api-keys/api';
import { apiKeyKeys } from '$lib/features/access/api-keys/apiKeyKeys';
import { nativeObject, parseNativeJSON } from '$lib/json/nativeJson';
import { abortError, errorMessage } from '$lib/api/http';
import {
  inspectRouting,
  runPlayground,
  streamPlayground,
  type InspectRoutingInput,
  type PlaygroundOperation,
  type PlaygroundRequest,
  type PlaygroundStreamDone
} from './api';
import { nativeDialects, nativeOperationRequest } from './nativeOperation';
import { playgroundTemplates, templateFor } from './templates';
import {
  parseMaxOutputTokens,
  parseResponseSchema,
  parseTemperature,
  parseTools
} from './validation';

type Mode = 'text' | 'tools' | 'structured';
type Composer = 'basic' | 'advanced';

// Create during component initialization so queries, effects, and cancellation
// share the lifetime of one mounted playground page.
export class PlaygroundState {
  routing = $state('{}');
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
    queryFn: ({ signal }) => listRoutes(signal)
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
  outputPolicyActive = $derived(
    hasOutputRules(
      this.selectedRoute?.latest_revision?.content_policy?.rules ?? []
    )
  );
  routeOperations = $derived(
    this.selectedRoute?.latest_revision?.operations ?? null
  );
  // Basic mode always composes a generation request; the Advanced operation is
  // kept for when the operator switches back.
  activeOperation = $derived<PlaygroundOperation>(
    this.composer === 'advanced' ? this.operation : 'generation'
  );
  operationKnown = $derived(
    this.routeOperations == null ||
      this.routeOperations.includes(this.operation)
  );
  streamSelectable = $derived(
    this.activeOperation === 'generation' && !this.outputPolicyActive
  );
  simulation = createMutation(() => ({
    // Wrapped so the mutation context is not passed as the abort signal.
    mutationFn: (input: InspectRoutingInput) => inspectRouting(input)
  }));
  simulateKeyId = $state('');
  simulateSeed = $state('');
  inspectDialect = $state('');
  private inspectedInputs = $state('');
  simulationError = $state('');
  // A dry run is the most recent action when its inputs still match; a run resets it.
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
    this.mutation.reset();
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

  // The inspector dialect only applies while it belongs to the current
  // operation and surface; a stale choice is dropped rather than sent.
  currentInspectDialect = (): string | undefined =>
    this.composer === 'advanced' &&
    (inspectionDialects(this.operation, this.surface) as string[]).includes(
      this.inspectDialect
    )
      ? this.inspectDialect
      : undefined;

  currentNativeDialect = (): string => {
    const options = nativeDialects(this.operation);
    return options.includes(this.nativeDialect)
      ? this.nativeDialect
      : (options[0] ?? '');
  };

  private advancedRequest(): Record<string, unknown> {
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

  explain = async () => {
    this.simulationError = '';
    if (!this.model.trim()) {
      this.simulationError = 'Enter an active route slug.';
      return;
    }
    try {
      const version = this.inspectionInputs();
      const registeredNative =
        this.strictSelected &&
        this.composer === 'advanced' &&
        nativeDialects(this.operation).length > 0;
      const selectedDialect = registeredNative
        ? this.currentNativeDialect()
        : this.currentInspectDialect();
      const input: InspectRoutingInput = {
        route: this.model.trim(),
        operation: this.activeOperation,
        surface: registeredNative ? 'native' : this.surface,
        mode:
          this.activeOperation === 'realtime'
            ? 'realtime'
            : registeredNative
              ? 'unary'
              : this.streamEnabled
                ? 'streaming'
                : 'unary',
        preferences: JSON.parse(this.routing),
        apiKeyId: this.simulateKeyId || null,
        seed: this.simulateSeed,
        clientContract:
          registeredNative && this.operation === 'embeddings'
            ? 'raw-vector-storage/1'
            : undefined,
        ...(this.composer === 'advanced' && this.operation !== 'realtime'
          ? {
              request: registeredNative
                ? nativeOperationRequest(
                    this.rawJson,
                    this.model.trim(),
                    selectedDialect!
                  )
                : this.advancedRequest(),
              dialect: selectedDialect as
                InspectRoutingInput['dialect'] | undefined
            }
          : {})
      };
      await this.simulation.mutateAsync(input);
      this.inspectedInputs = version;
    } catch (error) {
      this.simulationError = errorMessage(
        error,
        'The routing explanation could not be produced.'
      );
    }
  };

  private async runStream(request: PlaygroundRequest) {
    const abort = new AbortController();
    this.streamAbort = abort;
    this.mutation.reset();
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

  submit = async (event: SubmitEvent) => {
    event.preventDefault();
    this.validationError = '';
    // The run that follows produces its own explanation, so the dry run stops
    // competing for the panel.
    this.simulation.reset();
    this.simulationError = '';
    if (!this.model.trim()) {
      this.validationError = 'Enter an active route slug.';
      return;
    }
    if (this.composer === 'advanced' && !this.operationKnown) {
      this.validationError = `The route does not offer the ${this.operation} operation.`;
      return;
    }
    if (this.streamEnabled && this.streamCheck !== 'ok') {
      this.validationError =
        'Streaming has not been verified as available for this route.';
      return;
    }
    if (this.strictSelected) {
      this.validationError =
        'Use the qualified public client below for this strict route.';
      return;
    }
    if (this.activeOperation === 'translation') {
      this.validationError = 'Use the audio upload form below.';
      return;
    }
    if (this.activeOperation === 'realtime') {
      this.validationError =
        'Use the local realtime event viewer below; it does not open a provider session.';
      return;
    }
    if (
      this.activeOperation === 'classification' ||
      this.activeOperation === 'scoring'
    ) {
      this.validationError =
        'This operation requires a strict registered native route and the public client below.';
      return;
    }
    let request: PlaygroundRequest;
    try {
      if (this.composer === 'advanced') {
        request = {
          routing: JSON.parse(this.routing),
          model: this.model.trim(),
          surface: this.surface,
          operation: this.activeOperation,
          request: this.advancedRequest(),
          stream: this.streamEnabled ? true : undefined
        };
      } else {
        if (!this.input.trim()) {
          this.validationError = 'Enter a prompt.';
          return;
        }
        request = {
          routing: JSON.parse(this.routing),
          model: this.model.trim(),
          input: this.input,
          surface: this.surface,
          stream: this.streamEnabled ? true : undefined,
          ...this.requestControls()
        };
      }
    } catch (error) {
      this.validationError = errorMessage(error, 'Check the request fields.');
      return;
    }
    this.streamFrames = [];
    this.streamDone = null;
    this.streamProblem = null;
    this.mutation.reset();
    this.completedRequest = request;
    if (this.streamEnabled) {
      await this.runStream(request);
      return;
    }
    // A transport or route failure is rendered by the result panel; rethrowing
    // here would leave an unhandled rejection with nothing to catch it.
    try {
      await this.mutation.mutateAsync(request);
    } catch {
      this.validationError = '';
    }
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
