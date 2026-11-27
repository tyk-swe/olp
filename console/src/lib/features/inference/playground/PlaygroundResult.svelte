<script lang="ts">
  import { copyText } from '#lib/clipboard.js';
  import Callout from '#lib/components/Callout.svelte';
  import EmptyState from '#lib/components/EmptyState.svelte';
  import { notify } from '#lib/components/toasts.svelte.js';
  import { formatInteger } from '#lib/format.js';
  import { errorMessage } from '#lib/management/http.js';
  import OperationResult from './OperationResult.svelte';
  import type { PlaygroundState } from './playgroundState.svelte';

  let { playground }: { playground: PlaygroundState } = $props();

  let copiedKey = $state('');
  let copyTimer: ReturnType<typeof setTimeout> | undefined;
  $effect(() => () => copyTimer && clearTimeout(copyTimer));

  async function copyOutput(key: string, value: string) {
    if (!(await copyText(value))) {
      notify('Clipboard access is unavailable.', 'warning');
      return;
    }
    copiedKey = key;
    if (copyTimer) clearTimeout(copyTimer);
    copyTimer = setTimeout(() => (copiedKey = ''), 1800);
  }

  const hasResultContent = $derived(
    Boolean(
      playground.mutation.data?.refusal ||
      playground.mutation.data?.output_text ||
      playground.mutation.data?.tool_calls?.length ||
      playground.mutation.data?.response !== undefined ||
      (playground.mutation.data?.structured_output !== undefined &&
        playground.mutation.data?.structured_output !== null)
    )
  );
</script>

<section class="card result" aria-labelledby="result-title" aria-live="polite">
  <div class="result-heading">
    <div>
      <p class="eyebrow">Ephemeral response</p>
      <h2 id="result-title">Result</h2>
    </div>
    {#if playground.mutation.data && !playground.mutation.isPending}<span
        class="badge success"
      >
        {playground.mutation.data.latency_ms} ms
      </span>{/if}
  </div>
  {#if playground.streaming || playground.streamText || playground.streamDone || playground.streamProblem}
    {#if playground.streamProblem}<div class="output">
        <Callout tone="danger">{playground.streamProblem}</Callout>
      </div>{/if}
    {#if playground.streamText}<div class="output">
        <div class="output-head">
          <h3>Frames</h3>
          <button
            class="button button-quiet copy-button"
            type="button"
            onclick={() => copyOutput('stream', playground.streamText)}
          >
            {copiedKey === 'stream' ? 'Copied' : 'Copy'}
          </button>
        </div>
        <pre data-testid="stream-frames">{playground.streamText}</pre>
      </div>{/if}
    {#if playground.streaming}<div class="loading-state" role="status">
        Streaming response…
      </div>{/if}
    {#if playground.streamDone}<dl>
        <div>
          <dt>Response ID</dt>
          <dd class="mono">{playground.streamDone.id ?? 'Not reported'}</dd>
        </div>
        <div>
          <dt>Route</dt>
          <dd>{playground.streamDone.model ?? 'Not reported'}</dd>
        </div>
        <div>
          <dt>Input tokens</dt>
          <dd>{formatInteger(playground.streamDone.usage?.input_tokens)}</dd>
        </div>
        <div>
          <dt>Output tokens</dt>
          <dd>{formatInteger(playground.streamDone.usage?.output_tokens)}</dd>
        </div>
        <div>
          <dt>Total tokens</dt>
          <dd>{formatInteger(playground.streamDone.usage?.total_tokens)}</dd>
        </div>
      </dl>{/if}
  {:else if playground.mutation.isPending}<div
      class="loading-state"
      role="status"
    >
      Waiting for the route…
    </div>
  {:else if playground.mutation.isError}<div class="output">
      <Callout tone="danger">
        {errorMessage(
          playground.mutation.error,
          'The playground request failed.'
        )}
      </Callout>
    </div>
  {:else if playground.mutation.data}
    {#if playground.mutation.data.refusal}
      <div class="output" role="alert">
        <Callout tone="warning" title="The model refused this request">
          <p>{playground.mutation.data.refusal}</p>
        </Callout>
      </div>
    {/if}
    {#if !hasResultContent}
      <div class="output">
        <EmptyState title="No content returned">
          The route answered without text, a tool call, or structured output.
          The finish reason below says why.
        </EmptyState>
      </div>
    {/if}
    {#if playground.mutation.data.response !== undefined}<div class="output">
        <OperationResult
          operation={playground.completedRequest?.operation ?? 'generation'}
          response={playground.mutation.data.response}
          responseRaw={playground.mutation.data.response_raw}
          request={playground.completedRequest?.request}
        />
      </div>{/if}
    {#if playground.mutation.data.output_text}<div class="output">
        <div class="output-head">
          <h3>Text</h3>
          <button
            class="button button-quiet copy-button"
            type="button"
            onclick={() =>
              copyOutput('text', playground.mutation.data?.output_text ?? '')}
          >
            {copiedKey === 'text' ? 'Copied' : 'Copy'}
          </button>
        </div>
        <pre>{playground.mutation.data.output_text}</pre>
      </div>{/if}
    {#if playground.mutation.data.tool_calls?.length}{@const toolJson =
        JSON.stringify(playground.mutation.data.tool_calls, null, 2)}
      <div class="output">
        <div class="output-head">
          <h3>Tool calls</h3>
          <button
            class="button button-quiet copy-button"
            type="button"
            onclick={() => copyOutput('tools', toolJson)}
          >
            {copiedKey === 'tools' ? 'Copied' : 'Copy'}
          </button>
        </div>
        <pre>{toolJson}</pre>
      </div>{/if}
    {#if playground.mutation.data.structured_output !== undefined && playground.mutation.data.structured_output !== null}{@const structuredJson =
        JSON.stringify(playground.mutation.data.structured_output, null, 2)}
      <div class="output">
        <div class="output-head">
          <h3>Structured output</h3>
          <button
            class="button button-quiet copy-button"
            type="button"
            onclick={() => copyOutput('structured', structuredJson)}
          >
            {copiedKey === 'structured' ? 'Copied' : 'Copy'}
          </button>
        </div>
        <pre>{structuredJson}</pre>
      </div>{/if}
    <dl>
      <div>
        <dt>Response ID</dt>
        <dd class="mono">{playground.mutation.data.id}</dd>
      </div>
      <div>
        <dt>Route</dt>
        <dd>{playground.mutation.data.model}</dd>
      </div>
      <div>
        <dt>Provider model</dt>
        <dd>{playground.mutation.data.provider_model ?? 'Not reported'}</dd>
      </div>
      <div>
        <dt>Finish reason</dt>
        <dd>{playground.mutation.data.finish_reason ?? 'Not reported'}</dd>
      </div>
      <div>
        <dt>Input tokens</dt>
        <dd>{formatInteger(playground.mutation.data.usage?.input_tokens)}</dd>
      </div>
      <div>
        <dt>Cached input tokens</dt>
        <dd>
          {formatInteger(playground.mutation.data.usage?.cached_input_tokens)}
        </dd>
      </div>
      <div>
        <dt>Output tokens</dt>
        <dd>{formatInteger(playground.mutation.data.usage?.output_tokens)}</dd>
      </div>
      <div>
        <dt>Reasoning tokens</dt>
        <dd>
          {formatInteger(playground.mutation.data.usage?.reasoning_tokens)}
        </dd>
      </div>
      <div>
        <dt>Total tokens</dt>
        <dd>{formatInteger(playground.mutation.data.usage?.total_tokens)}</dd>
      </div>
    </dl>
  {:else}
    <div class="output">
      <EmptyState title="Ready to test">
        Choose a mode, enter an active route slug, and run an ephemeral request.
      </EmptyState>
    </div>
  {/if}
</section>

<style>
  .result {
    padding: 1.5rem;
  }
  .result-heading {
    display: flex;
    justify-content: space-between;
    align-items: flex-start;
    gap: 1rem;
  }
  h2 {
    margin: 0;
    font-size: var(--text-body);
    font-weight: 500;
    letter-spacing: -0.02em;
  }
  .output-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 0.75rem;
    margin-bottom: 0.5rem;
  }
  h3 {
    margin: 0;
    color: var(--foreground-subtle);
    font-family: var(--font-mono);
    font-size: var(--text-caption);
    font-weight: 400;
    letter-spacing: -0.02em;
    text-transform: uppercase;
  }
  .copy-button {
    min-height: 2rem;
    padding: 0.25rem 0.6rem;
    font-size: var(--text-caption);
  }
  .output {
    margin-top: 1rem;
  }
  pre {
    max-height: 28rem;
    margin: 0;
    overflow: auto;
    padding: 1rem;
    border-radius: var(--radius-control);
    background: var(--surface-raised);
    font-family: var(--font-mono);
    font-size: var(--text-caption);
    line-height: 1.5;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
  dl {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 0.75rem;
    margin: 1rem 0 0;
    padding-top: 1rem;
    border-top: 1px solid var(--border-hairline);
  }
  dt {
    color: var(--foreground-subtle);
    font-family: var(--font-mono);
    font-size: var(--text-caption);
    font-weight: 400;
    letter-spacing: -0.02em;
    text-transform: uppercase;
  }
  dd {
    margin: 0.25rem 0 0;
    overflow-wrap: anywhere;
  }
  @media (max-width: 38rem) {
    .result {
      padding: 1rem;
    }
    dl {
      grid-template-columns: 1fr;
    }
  }
</style>
