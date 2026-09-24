<script lang="ts">
  import { parseRealtimeTrace, type RealtimeEventRow } from './realtimeTrace';

  let source = $state(
    '[\n  {"type":"input_audio_buffer.speech_started","audio_start_ms":120},\n  {"type":"input_audio_buffer.speech_stopped","audio_end_ms":840},\n  {"type":"response.created"},\n  {"type":"response.done"}\n]'
  );
  let rows = $state<RealtimeEventRow[]>([]);
  let error = $state('');

  function inspect() {
    error = '';
    try {
      rows = parseRealtimeTrace(source);
    } catch (reason) {
      rows = [];
      error = reason instanceof Error ? reason.message : 'Invalid event trace.';
    }
  }
</script>

<section class="card trace" aria-labelledby="realtime-trace-heading">
  <p class="eyebrow">Local trace inspection · no inference</p>
  <h2 id="realtime-trace-heading">Realtime event timeline</h2>
  <p>
    Paste a native event array to inspect turn order, VAD, interruption and
    timing. This local view does not open a realtime session or send event
    content to the gateway. The browser API cannot supply the bearer WebSocket
    header required by the public realtime endpoint; use a supported SDK to
    capture the trace.
  </p>
  <div class="form-field">
    <label for="realtime-events">Native event JSON</label>
    <textarea
      id="realtime-events"
      bind:value={source}
      rows="9"
      spellcheck="false"></textarea>
  </div>
  <button class="button button-secondary" type="button" onclick={inspect}>
    Inspect event order locally
  </button>
  {#if error}<p class="inline-problem" role="alert">{error}</p>{/if}
  {#if rows.length}
    <ol aria-label="Realtime event sequence">
      {#each rows as row (row.position)}
        <li>
          <strong>{row.position + 1}. {row.phase}</strong>
          <span>Role: {row.role}</span>
          {#if row.timing.length}<span>{row.timing.join(' · ')}</span>{/if}
        </li>
      {/each}
    </ol>
    <p class="scope-note">
      Text, audio bytes, event IDs, tool arguments and opaque state are hidden
      from this timeline. Unknown event types remain unknown.
    </p>
  {/if}
</section>

<style>
  .trace {
    display: grid;
    gap: 0.8rem;
    padding: 1.5rem;
    margin-top: 1rem;
  }
  h2,
  p {
    margin: 0;
  }
  h2 {
    font-size: 1.15rem;
  }
  ol {
    margin: 0;
    padding-left: 1.2rem;
  }
  li {
    padding: 0.4rem 0;
  }
  li span {
    display: block;
    color: var(--foreground-muted);
    font-size: var(--text-caption);
  }
  .scope-note {
    color: var(--foreground-muted);
    font-size: var(--text-caption);
  }
</style>
