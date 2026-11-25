<script lang="ts">
  import Callout from '#lib/components/Callout.svelte';
  import { useMutation } from '#lib/editing/action.svelte.js';
  import { notify } from '#lib/components/toasts.svelte.js';
  import { updateNotificationRule, type NotificationRule } from './api';

  let {
    rule,
    disabled,
    saved
  }: {
    rule: NotificationRule;
    disabled: boolean;
    saved: (rule: NotificationRule) => Promise<void>;
  } = $props();

  const action = useMutation();
  let startsAt = $state('');
  let endsAt = $state('');
  let now = $state(Date.now());
  const silence = $derived(rule.silence);
  const status = $derived(
    !silence
      ? 'No silence'
      : now < Date.parse(silence.starts_at)
        ? 'Scheduled'
        : now < Date.parse(silence.ends_at)
          ? 'Active'
          : 'Expired'
  );

  $effect(() => {
    if (!silence) return;
    const timer = setInterval(() => (now = Date.now()), 1000);
    return () => clearInterval(timer);
  });

  const utcLabel = (date: string) =>
    date.replace('T', ' ').replace(/(?:\.0+)?Z$/, ' UTC');

  async function setSilence(window: NotificationRule['silence']) {
    await action.submit(
      { current: rule, patch: { silence: window } },
      ({ current, patch }, key) => updateNotificationRule(current, patch, key),
      async (value) => {
        now = Date.now();
        startsAt = endsAt = '';
        notify(window ? 'Silence saved.' : 'Silence canceled.');
        await saved(value);
      }
    );
  }

  function schedule(event: SubmitEvent) {
    event.preventDefault();
    const start = new Date(`${startsAt}Z`);
    const end = new Date(`${endsAt}Z`);
    if (!(end.valueOf() > start.valueOf())) {
      action.error = 'Enter an end time after the start time.';
      return;
    }
    void setSilence({
      starts_at: start.toISOString(),
      ends_at: end.toISOString()
    });
  }
</script>

<section class="silence-editor" aria-labelledby="rule-silence-title">
  <h3 id="rule-silence-title">Notification silence</h3>
  <p class="section-help">
    Pause new triggers and repeats for this alert only. Evaluation and delivery
    history continue. Previously queued deliveries and their recoveries still
    send.
  </p>
  <p class="silence-status" aria-live="polite">
    <strong>{status}</strong>
    {#if silence}
      <span>
        Starts <time datetime={silence.starts_at}>
          {utcLabel(silence.starts_at)}
        </time>
      </span>
      <span>
        Ends <time datetime={silence.ends_at}>{utcLabel(silence.ends_at)}</time>
      </span>
    {/if}
  </p>
  {#if action.error}<Callout tone="danger">{action.error}</Callout>{/if}
  {#if status === 'Scheduled' || status === 'Active'}
    <button
      class="button"
      type="button"
      disabled={disabled || action.busy}
      onclick={() => setSilence(null)}
    >
      Cancel silence
    </button>
  {:else}
    <form class="silence-fields" onsubmit={schedule}>
      <label class="form-field">
        Start (UTC)
        <input
          type="datetime-local"
          required
          bind:value={startsAt}
          disabled={disabled || action.busy}
        />
      </label>
      <label class="form-field">
        End (UTC)
        <input
          type="datetime-local"
          required
          bind:value={endsAt}
          disabled={disabled || action.busy}
        />
      </label>
      <button
        class="button button-primary"
        type="submit"
        disabled={disabled || action.busy}
      >
        Schedule silence
      </button>
    </form>
  {/if}
</section>

<style>
  .silence-editor {
    display: grid;
    justify-items: start;
    gap: 0.75rem;
    padding: 0.75rem;
  }

  .silence-editor h3,
  .silence-editor p {
    margin: 0;
  }

  .silence-status,
  .silence-fields {
    display: flex;
    flex-wrap: wrap;
    align-items: end;
    gap: 0.75rem;
  }
</style>
