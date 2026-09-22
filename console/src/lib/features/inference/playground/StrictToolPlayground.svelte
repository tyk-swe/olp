<script lang="ts">
  import { onDestroy } from 'svelte';
  import {
    nativeChatRequest,
    nextTurn,
    recoverTurn,
    streamTurn,
    submissionID,
    unaryTurn,
    type ReadyTurn
  } from './browserContinuation';

  let { route, requestText }: { route: string; requestText: string } = $props();
  let key = $state('');
  let busy = $state('');
  let error = $state('');
  let turns = $state<ReadyTurn[]>([]);
  let activeIndex = $state(0);
  let results = $state<string[]>([]);
  let pending = $state<{
    submission: string;
    request: ReadyTurn['request'];
  } | null>(null);
  let abort: AbortController | null = null;
  let epoch = 0;

  const active = $derived(turns[activeIndex]);
  const calls = $derived(active?.assistant.tool_calls ?? []);

  function problem(reason: unknown) {
    return reason instanceof Error
      ? reason.message
      : 'The public inference request could not be completed.';
  }

  function reset() {
    epoch += 1;
    abort?.abort();
    abort = null;
    error = '';
    busy = '';
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
    if (busy) return;
    reset();
    const owner = epoch;
    let request: ReadyTurn['request'];
    try {
      request = nativeChatRequest(requestText, route);
    } catch (reason) {
      error = problem(reason);
      return;
    }
    const submission = submissionID();
    pending = { submission, request };
    busy = 'first';
    abort = new AbortController();
    try {
      const turn = await streamTurn(key, request, submission, abort.signal);
      if (owner === epoch) addTurn(turn);
    } catch (reason) {
      if (owner === epoch) error = problem(reason);
    } finally {
      if (owner === epoch) {
        busy = '';
        abort = null;
      }
    }
  }

  async function continueWithResults() {
    if (busy || !active || !calls.length) return;
    const owner = epoch;
    let request: ReadyTurn['request'];
    try {
      request = nextTurn(
        active,
        calls.map((call, index) => ({
          tool_call_id: call.id,
          content: results[index] ?? ''
        }))
      );
    } catch (reason) {
      error = problem(reason);
      return;
    }
    const submission = submissionID();
    pending = { submission, request };
    busy = 'next';
    error = '';
    abort = new AbortController();
    try {
      const turn = await unaryTurn(
        key,
        request,
        submission,
        active.handle,
        abort.signal
      );
      if (owner === epoch) addTurn(turn);
    } catch (reason) {
      if (owner === epoch) error = problem(reason);
    } finally {
      if (owner === epoch) {
        busy = '';
        abort = null;
      }
    }
  }

  async function recover() {
    if (busy || !pending) return;
    const owner = epoch;
    busy = 'recover';
    error = '';
    abort = new AbortController();
    try {
      const turn = await recoverTurn(
        key,
        pending.submission,
        pending.request,
        abort.signal
      );
      if (owner === epoch) addTurn(turn);
    } catch (reason) {
      if (owner === epoch) error = problem(reason);
    } finally {
      if (owner === epoch) {
        busy = '';
        abort = null;
      }
    }
  }

  function select(index: number) {
    activeIndex = index;
    results = turns[index]?.assistant.tool_calls?.map(() => '') ?? [];
    error = '';
  }

  onDestroy(() => {
    epoch += 1;
    abort?.abort();
    key = '';
  });
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
  <div class="form-field">
    <label for="strict-api-key"
      >Inference API key with provider-state permission</label
    >
    <input
      id="strict-api-key"
      type="password"
      bind:value={key}
      autocomplete="off"
      spellcheck="false"
      aria-describedby="strict-key-help"
    />
    <small id="strict-key-help">
      Use a key allowed for this route with inference and provider-state
      permission. It stays in this tab and is sent only to the same-origin
      public API.
    </small>
  </div>
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
      disabled={Boolean(busy) || !key || !route}
      >{busy === 'first'
        ? 'Waiting for ready continuation…'
        : 'Run strict first turn'}</button
    >
    {#if busy}<button
        type="button"
        class="button button-secondary"
        onclick={() => abort?.abort()}>Cancel delivery</button
      >{/if}
    {#if turns.length || pending}<button
        type="button"
        class="button button-secondary"
        onclick={reset}>Clear interaction</button
      >{/if}
  </div>
  {#if error}<p class="inline-problem" role="alert">{error}</p>{/if}
  {#if pending && !busy}
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
        disabled={!key}>Recover committed delivery</button
      >
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
          >Turn {index + 1}{turn.assistant.tool_calls?.length
            ? ' · tools'
            : ' · final'}</button
        >
      {/each}
    </div>
  {/if}
  {#if active}
    <div class="ready" role="status">
      <strong>Recoverable continuation ready</strong>
      <span
        >Tool actions became available after the encrypted state commit.</span
      >
    </div>
    {#if active.observations.length}
      <div>
        <h3>Ordered client observations</h3>
        <ol class="observations">
          {#each active.observations as observation, index (index)}
            <li>
              <strong
                >{observation.type.replaceAll('_', ' ')} · {observation.phase}</strong
              >
              {#if observation.type === 'thinking' || observation.type === 'redacted_thinking'}
                <span
                  >Opaque reasoning dependency retained; content hidden.</span
                >
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
            <label for={`strict-tool-result-${index}`}
              >Result for call {index + 1}</label
            >
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
          disabled={Boolean(busy) ||
            !key ||
            results.some((value) => !value?.trim())}
          >{busy === 'next'
            ? 'Submitting tool results…'
            : 'Submit ordered tool results'}</button
        >
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
