<script lang="ts">
  import type { MediaJob } from './api';
  import { mediaTimeline } from './mediaTimeline';
  import { formatDate } from '$lib/format';

  let { job }: { job: MediaJob } = $props();
  const timeline = $derived(mediaTimeline(job));
</script>

<section class="media-timeline" aria-labelledby="media-timeline-heading">
  <h3 id="media-timeline-heading">Recorded media lifecycle</h3>
  <ol>
    {#each timeline.recorded as event (`${event.label}-${event.at}`)}
      <li>
        <strong>{event.label}</strong>
        <time datetime={event.at}>{formatDate(event.at)}</time>
        <span>{event.detail}</span>
      </li>
    {/each}
  </ol>
  {#if timeline.expiry}<p>
      Retention deadline: <time datetime={timeline.expiry}
        >{formatDate(timeline.expiry)}</time
      >. This is a scheduled boundary, not a completion event.
    </p>{/if}
  <p>
    Native media bytes, codecs, frame timing and masks stay with the authorized
    provider resource. This metadata view does not download or transform them.
  </p>
</section>

<style>
  .media-timeline {
    border-top: 1px solid var(--border-hairline);
    padding-top: 0.8rem;
  }
  h3,
  p {
    margin: 0 0 0.6rem;
  }
  h3 {
    font-size: 0.95rem;
  }
  ol {
    margin: 0 0 0.6rem;
    padding-left: 1.2rem;
  }
  li {
    padding: 0.35rem 0;
  }
  li time,
  li span {
    display: block;
    color: var(--foreground-muted);
    font-size: var(--text-caption);
  }
  p {
    color: var(--foreground-muted);
    font-size: var(--text-caption);
  }
</style>
