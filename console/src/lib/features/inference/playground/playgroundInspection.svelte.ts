import { useAction } from '#lib/editing/action.svelte.js';
import { replaceNative, type NativeObject } from '#lib/nativeJson.js';
import { inspectRouting, type InspectRoutingInput } from './api';
import { nativeOperationRequest } from './nativeOperation';
import type { PlaygroundState } from './playgroundState.svelte';

type RoutingDecisions = Awaited<ReturnType<typeof inspectRouting>>;

export class PlaygroundInspection {
  readonly action = useAction();
  keyId = $state('');
  seed = $state('');
  private decisions = $state<RoutingDecisions | null>(null);
  private inspectedInputs = $state('');
  current = $derived(
    this.decisions?.length && this.inspectedInputs === this.inputs()
      ? this.decisions
      : null
  );

  constructor(private readonly playground: PlaygroundState) {}

  private inputs() {
    const playground = this.playground;
    return JSON.stringify([
      playground.model,
      playground.surface,
      playground.operation,
      playground.composer,
      playground.rawJson,
      playground.streamEnabled,
      playground.routing,
      this.keyId,
      this.seed,
      playground.inspectDialect,
      playground.nativeDialect
    ]);
  }

  private nativeRequest(dialect: string): NativeObject {
    const request = nativeOperationRequest(
      this.playground.rawJson,
      this.playground.model.trim(),
      dialect
    );
    return dialect === 'openai-completions'
      ? (replaceNative(
          request,
          ['stream'],
          this.playground.streamEnabled
        ) as NativeObject)
      : request;
  }

  private request(): InspectRoutingInput {
    const playground = this.playground;
    const plan = playground.plan;
    return {
      route: playground.model.trim(),
      operation: playground.activeOperation,
      surface: plan.inspection.surface,
      mode: plan.inspection.mode,
      preferences: JSON.parse(playground.routing),
      apiKeyId: this.keyId || null,
      seed: this.seed,
      clientContract: plan.inspection.clientContract,
      ...(playground.composer === 'advanced' && plan.kind !== 'realtime'
        ? {
            request:
              plan.inspection.registeredNative ||
              plan.inspection.dialect === 'openai-completions'
                ? this.nativeRequest(plan.inspection.dialect ?? '')
                : playground.advancedRequest(),
            dialect: plan.inspection.dialect
          }
        : {})
    };
  }

  reset = () => {
    this.action.retire();
    this.decisions = null;
  };

  explain = async () => {
    if (!this.playground.model.trim()) {
      this.action.error = 'Enter an active route slug.';
      return;
    }
    const version = this.inputs();
    await this.action.run(
      async () => {
        const input = this.request();
        this.decisions = null;
        return inspectRouting(input);
      },
      (decisions) => {
        this.decisions = decisions;
        this.inspectedInputs = version;
      },
      { fallback: 'The routing explanation could not be produced.' }
    );
  };
}
