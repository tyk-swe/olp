<script lang="ts">
  import PublicKeyField from './PublicKeyField.svelte';
  import {
    nativeChatRequest,
    nextTurn,
    recoverTurn,
    streamTurn,
    submissionID,
    unaryTurn,
    type ReadyTurn
  } from './browserContinuation';
  import { usePublicClientAction } from './usePublicClientAction.svelte';

  let { route, requestText }: { route: string; requestText: string } = $props();
  let key = $state('');
  const client = usePublicClientAction(
    'The public inference request could not be completed.',
    () => {
      key = '';
    }
  );
  let turns = $state<ReadyTurn[]>([]);
  let activeIndex = $state(0);
  let results = $state<string[]>([]);
  let pending = $state<{
    submission: string;
    request: ReadyTurn['request'];
  } | null>(null);

  const active = $derived(turns[activeIndex]);
  const calls = $derived(active?.assistant.tool_calls ?? []);

  function reset() {
    client.reset();
    pending = null;
    turns = [];
    results = [];
    activeIndex = 0;
  }

  function addTurn(turn: ReadyTurn) {
    turns = [...turns, turn];
    activeIndex = turns.length - 1;
    results = turn.assistant.tool_calls?.map(() => '') ?? [];
    pending = null;
  }

  async function begin() {
    if (client.busy) return;
    reset();
    const built = nativeChatRequest(requestText, route);
    if (!built.ok) {
      client.refuse(built.error);
      return;
    }
    const request = built.value;
    const submission = submissionID();
    pending = { submission, request };
    await client.run(
      (signal) => streamTurn(key, request, submission, signal),
      addTurn,
      'first'
    );
  }

  async function continueWithResults() {
    if (client.busy || !active || !calls.length) return;
    const built = nextTurn(
      active,
      calls.map((call, index) => ({
        tool_call_id: call.id,
        content: results[index] ?? ''
      }))
    );
    if (!built.ok) {
      client.refuse(built.error);
      return;
    }
    const request = built.value;
    const submission = submissionID();
    pending = { submission, request };
    const handle = active.handle;
    await client.run(
      (signal) => unaryTurn(key, request, submission, handle, signal),
      addTurn,
      'next'
    );
  }

  async function recover() {
    if (client.busy || !pending) return;
    const pendingTurn = pending;
    await client.run(
      (signal) =>
        recoverTurn(key, pendingTurn.submission, pendingTurn.request, signal),
      addTurn,
      'recover'
    );
  }

  function select(index: number) {
    activeIndex = index;
    results = turns[index]?.assistant.tool_calls?.map(() => '') ?? [];
    client.clearError();
  }
</script>

<section class="card strict-client" aria-labelledby="strict-client-heading">
  <p class="eyebrow">Public inference client · negotiated</p>
  <h2 id="strict-client-heading">Complete a strict tool interaction</h2>
  <p>
    This client uses the versioned OpenAI Chat to Anthropic tool continuation
    contract. It retains the request and ordered observations in this tab, waits
    for encrypted state to become ready, and sends one result for each tool
    call. It does not execute tools for you.
  </p>
  <PublicKeyField
    id="strict-api-key"
    label="Inference API key with provider-state permission"
    help="Use a key allowed for this route with inference and provider-state permission. It stays in this tab and is sent only to the same-origin public API."
    bind:value={key}
  />
  <p class="scope-note">
    The Advanced request JSON above is the complete native Chat request. Its
    model must be empty or name <code>{route}</code>. The first turn streams;
    later tool results use the exact retained history and prior handle.
  </p>
  <div class="actions">
    <button
      type="button"
      class="button button-primary"
      onclick={begin}
      disabled={client.busy || !key || !route}
    >
      {client.key === 'first'
        ? 'Waiting for ready continuation…'
        : 'Run strict first turn'}
    </button>
    {#if client.busy}<button
        type="button"
        class="button button-secondary"
        onclick={client.cancel}
      >
        Cancel delivery
      </button>{/if}
    {#if turns.length || pending}<button
        type="button"
        class="button button-secondary"
        onclick={reset}
      >
        Clear interaction
      </button>{/if}
  </div>
  {#if client.error}<p class="inline-problem" role="alert">
      {client.error}
    </p>{/if}
  {#if pending && !client.busy}
    <div class="recovery" role="status">
      <p>
        The request may have reached the provider. Recover the same submission
        to check for a committed delivery; starting a new one would be new
        inference.
      </p>
      <button
        type="button"
        class="button button-secondary"
        onclick={recover}
        disabled={!key}
      >
        Recover committed delivery
      </button>
    </div>
  {/if}
  {#if turns.length > 1}
    <div class="branches" aria-label="Ready continuation branches">
      {#each turns as turn, index (turn.submission)}
        <button
          type="button"
          class="button button-secondary"
          aria-pressed={activeIndex === index}
          onclick={() => select(index)}
        >
          Turn {index + 1}{turn.assistant.tool_calls?.length
            ? ' · tools'
            : ' · final'}
        </button>
      {/each}
    </div>
  {/if}
  {#if active}
    <div class="ready" role="status">
      <strong>Recoverable continuation ready</strong>
      <span>
        Tool actions became available after the encrypted state commit.
      </span>
    </div>
    {#if active.observations.length}
      <div>
        <h3>Ordered client observations</h3>
        <ol class="observations">
          {#each active.observations as observation, index (index)}
            <li>
              <strong>
                {observation.type.replaceAll('_', ' ')} · {observation.phase}
              </strong>
              {#if observation.type === 'thinking' || observation.type === 'redacted_thinking'}
                <span>
                  Opaque reasoning dependency retained; content hidden.
                </span>
              {:else if observation.type === 'text' && observation.text}
                <span>{observation.text}</span>
              {:else if observation.type === 'tool_use' && observation.name}
                <span>{observation.name}</span>
              {/if}
            </li>
          {/each}
        </ol>
      </div>
    {/if}
    <div class="assistant-result">
      <h3>Assistant text</h3>
      <pre>{active.assistant.content || 'No ordinary text'}</pre>
      <p>Finish reason: {active.finish}</p>
      {#if active.nativeUsageRaw}
        <details>
          <summary>Native provider usage categories</summary>
          <pre>{active.nativeUsageRaw}</pre>
        </details>
      {/if}
    </div>
    {#if calls.length}
      <div>
        <h3>Ready tool calls</h3>
        <p>
          Results are paired with these calls in order. Changing a result
          creates a new continuation branch from this ready turn.
        </p>
        {#each calls as call, index (call.id)}
          <div class="tool-call">
            <strong>Call {index + 1} · {call.function.name}</strong>
            <details>
              <summary>Arguments</summary>
              <pre>{call.function.arguments}</pre>
            </details>
            <label for={`strict-tool-result-${index}`}>
              Result for call {index + 1}
            </label>
            <textarea
              id={`strict-tool-result-${index}`}
              bind:value={results[index]}
              rows="3"
              spellcheck="false"></textarea>
          </div>
        {/each}
        <button
          type="button"
          class="button button-primary"
          onclick={continueWithResults}
          disabled={client.busy ||
            !key ||
            results.some((value) => !value?.trim())}
        >
          {client.key === 'next'
            ? 'Submitting tool results…'
            : 'Submit ordered tool results'}
        </button>
      </div>
    {/if}
  {/if}
</section>

<style>
  .strict-client {
    display: grid;
    gap: 0.8rem;
    padding: 1.5rem;
    margin-top: 1rem;
  }
  h2,
  h3,
  p {
    margin: 0;
  }
  h2 {
    font-size: 1.15rem;
  }
  h3 {
    font-size: 0.94rem;
    margin-bottom: 0.4rem;
  }
  .scope-note,
  .recovery p,
  .ready span {
    color: var(--foreground-muted);
    font-size: var(--text-caption);
  }
  .actions,
  .branches {
    display: flex;
    flex-wrap: wrap;
    gap: 0.5rem;
  }
  .ready,
  .recovery,
  .tool-call {
    padding: 0.85rem;
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
  }
  .ready {
    border-color: var(--success);
    display: grid;
  }
  .observations {
    padding-left: 1.25rem;
    margin: 0;
  }
  .observations li {
    padding: 0.3rem 0;
  }
  .observations span {
    display: block;
    white-space: pre-wrap;
  }
  .tool-call {
    display: grid;
    gap: 0.45rem;
    margin: 0.5rem 0;
  }
  pre {
    overflow: auto;
    max-height: 20rem;
    font-family: var(--font-mono);
    font-size: var(--text-caption);
  }
  textarea {
    width: 100%;
  }
</style>
