<script lang="ts">
  import Callout from '#lib/components/Callout.svelte';
  import FormField from '#lib/components/FormField.svelte';
  import {
    useMutation,
    type ActionFeedback
  } from '#lib/editing/action.svelte.js';
  import { focusFormError } from '#lib/editing/focusError.js';
  import { notify } from '#lib/components/toasts.svelte.js';
  import { updateNotificationRule, type NotificationRule } from './api';
  import {
    silenceServerFields,
    validateSilence,
    type SilenceField
  } from './silenceDraft';

  let {
    rule,
    disabled,
    saved
  }: {
    rule: NotificationRule;
    disabled: boolean;
    saved: (rule: NotificationRule) => Promise<void>;
  } = $props();

  const action = useMutation<SilenceField>();
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

  function setSilence(
    window: NotificationRule['silence'],
    feedback?: ActionFeedback<SilenceField>
  ) {
    return action.submit(
      { current: rule, patch: { silence: window } },
      ({ current, patch }, key) => updateNotificationRule(current, patch, key),
      async (value) => {
        now = Date.now();
        startsAt = endsAt = '';
        notify(window ? 'Silence saved.' : 'Silence canceled.');
        await saved(value);
      },
      feedback
    );
  }

  async function schedule(
    event: SubmitEvent & { currentTarget: HTMLFormElement }
  ) {
    event.preventDefault();
    const form = event.currentTarget;
    const checked = validateSilence(startsAt, endsAt);
    if (!action.check(checked)) return focusFormError(form);
    const accepted = await setSilence(checked.value, {
      serverFields: silenceServerFields
    });
    if (!accepted) await focusFormError(form);
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
    <form class="silence-fields" novalidate onsubmit={schedule}>
      <FormField label="Start (UTC)" error={action.fieldErrors.startsAt}>
        {#snippet control(attributes)}
          <input
            {...attributes}
            type="datetime-local"
            required
            bind:value={startsAt}
            disabled={disabled || action.busy}
          />
        {/snippet}
      </FormField>
      <FormField label="End (UTC)" error={action.fieldErrors.endsAt}>
        {#snippet control(attributes)}
          <input
            {...attributes}
            type="datetime-local"
            required
            bind:value={endsAt}
            disabled={disabled || action.busy}
          />
        {/snippet}
      </FormField>
      <div class="page-actions">
        <button
          class="button button-primary"
          type="submit"
          disabled={disabled || action.busy}
        >
          Schedule silence
        </button>
      </div>
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

  .silence-fields {
    align-items: start;
  }

  .silence-fields .page-actions {
    flex-basis: 100%;
  }
</style>
