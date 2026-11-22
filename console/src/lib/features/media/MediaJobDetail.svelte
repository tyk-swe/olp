<script lang="ts">
  import { formatDate, stateLabel } from '#lib/format.js';
  import MediaTimeline from './MediaTimeline.svelte';
  import type { MediaJob, MediaContentVariant } from './api.js';

  let {
    job,
    canOperate,
    actionBusy,
    actionError,
    downloadVariant = $bindable(),
    onDownload,
    onRefresh,
    onRemove
  }: {
    job: MediaJob;
    canOperate: boolean;
    actionBusy: string | null;
    actionError: string | null;
    downloadVariant: MediaContentVariant;
    onDownload: () => void;
    onRefresh: () => void;
    onRemove: () => void;
  } = $props();

  function tone(state: string) {
    if (state === 'succeeded') return 'success';
    if (state === 'failed' || state === 'cancelled') return 'danger';
    return 'warning';
  }
</script>

<section class="card job-detail" aria-labelledby="job-state-heading">
  <div class="section-heading">
    <div>
      <p class="eyebrow">{job.operation}</p>
      <h2 id="job-state-heading">{job.route}</h2>
    </div>
    <span class={`badge ${tone(job.state)}`}>{job.state}</span>
  </div>
  <dl>
    <div>
      <dt>Lifecycle</dt>
      <dd>{stateLabel(job.lifecycle)}</dd>
    </div>
    <div>
      <dt>Progress</dt>
      <dd>
        {job.progress_percent == null ? '—' : `${job.progress_percent}%`}
      </dd>
    </div>
    <div>
      <dt>Provider</dt>
      <dd>
        {job.provider_name}<small>{job.provider_model}</small>
      </dd>
    </div>
    <div>
      <dt>Client surface</dt>
      <dd>{job.surface}</dd>
    </div>
    <div>
      <dt>Content status</dt>
      <dd>
        {job.content_available
          ? 'Available through the authenticated vendor API'
          : 'Not available'}
      </dd>
    </div>
    <div>
      <dt>Created</dt>
      <dd>{formatDate(job.created_at)}</dd>
    </div>
    <div>
      <dt>Completed</dt>
      <dd>
        {job.completed_at ? formatDate(job.completed_at) : 'Not finished'}
      </dd>
    </div>
    <div>
      <dt>Last polled</dt>
      <dd>
        {job.last_polled_at ? formatDate(job.last_polled_at) : 'Never polled'}
      </dd>
    </div>
    <div>
      <dt>Expires</dt>
      <dd>
        {job.expires_at ? formatDate(job.expires_at) : 'No retention deadline'}
      </dd>
    </div>
    <div>
      <dt>Deleted</dt>
      <dd>
        {job.deleted_at ? formatDate(job.deleted_at) : 'Not deleted'}
      </dd>
    </div>
    <div>
      <dt>Updated</dt>
      <dd>{formatDate(job.updated_at)}</dd>
    </div>
  </dl>
  <MediaTimeline {job} />
  {#if job.error_class || job.reconciliation_error}<div
      class="inline-problem"
      role="alert"
    >
      <strong>{job.error_class ?? 'Reconciliation error'}</strong>
      <p>
        {job.reconciliation_error ?? 'The upstream job failed.'}
      </p>
    </div>{/if}
  <div class="identifiers">
    <p>
      <strong>OLP job</strong>
      <code>{job.id}</code>
    </p>

    <p>
      <strong>Upstream job</strong>
      <code>{job.upstream_job_id ?? 'not assigned'}</code>
    </p>

    <p>
      <strong>API key ID</strong>
      <code>{job.api_key_id}</code>
    </p>

    <p>
      <strong>Provider ID</strong>
      <code>{job.provider_id}</code>
    </p>
  </div>
  {#if canOperate}
    <div class="job-actions">
      <button
        class="button button-secondary"
        type="button"
        onclick={onRefresh}
        disabled={actionBusy !== null}
        >{actionBusy === 'refresh' ? 'Refreshing…' : 'Refresh now'}</button
      >
      {#if job.content_available && job.state === 'succeeded'}
        <label class="variant-picker"
          >Variant
          <select bind:value={downloadVariant}>
            <option value="video">Video</option>
            <option value="thumbnail">Thumbnail</option>
            <option value="spritesheet">Spritesheet</option>
          </select></label
        >
        <button
          class="button button-secondary"
          type="button"
          onclick={onDownload}
          disabled={actionBusy !== null}
          >{actionBusy === 'download'
            ? 'Downloading…'
            : 'Download content'}</button
        >
      {/if}
      {#if job.lifecycle !== 'deleted'}
        <button
          class="button button-secondary button-danger"
          type="button"
          onclick={onRemove}
          disabled={actionBusy !== null}
          >{actionBusy === 'delete' ? 'Deleting…' : 'Delete content'}</button
        >
      {/if}
    </div>
  {/if}
  {#if actionError}<div class="inline-problem" role="alert">
      {actionError}
    </div>{/if}
</section>

<style>
  dd small {
    display: block;
  }
  dd small {
    color: var(--foreground-muted);
  }
  .job-detail {
    max-width: 64rem;
    margin-top: 1.5rem;
    padding: 1.5rem;
  }
  .section-heading {
    display: flex;
    align-items: start;
    justify-content: space-between;
    gap: 1rem;
  }
  h2 {
    margin: 0;
    font-size: 1.25rem;
    font-weight: 400;
    letter-spacing: -0.02em;
  }
  dl {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: 0.75rem;
    margin-top: 1.25rem;
  }
  dl div,
  .identifiers p {
    min-width: 0;
    padding: 0.75rem 1rem;
    border: 0;
    border-radius: var(--radius-control);
    background: var(--surface-raised);
  }
  dt,
  .identifiers strong {
    color: var(--foreground-subtle);
    font-family: var(--font-mono);
    font-size: var(--text-caption);
    font-weight: 400;
    letter-spacing: -0.02em;
    text-transform: uppercase;
  }
  dd {
    margin: 0.35rem 0 0;
    overflow-wrap: anywhere;
  }
  .identifiers {
    display: grid;
    gap: 0.5rem;
    margin-top: 1rem;
  }
  .identifiers p {
    display: grid;
    gap: 0.35rem;
    margin: 0;
  }
  .identifiers code {
    overflow-wrap: anywhere;
    font-family: var(--font-mono);
    font-size: var(--text-caption);
  }
  .job-actions {
    display: flex;
    flex-wrap: wrap;
    align-items: end;
    gap: 0.75rem;
    margin-top: 1rem;
  }
  .variant-picker {
    display: grid;
    gap: 0.4rem;
    font-size: var(--text-body-sm);
    font-weight: 500;
  }
  .variant-picker select {
    min-height: 2.5rem;
    padding: 0.5rem 0.75rem;
    border: 1px solid var(--border);
    border-radius: var(--radius-control);
    background: transparent;
    color: var(--foreground);
  }
  @media (max-width: 48rem) {
    dl {
      grid-template-columns: 1fr;
    }
  }
</style>
