import type { MediaJob } from './api';

export type MediaMilestone = {
  label: string;
  at: string;
  detail: string;
};

/** Record only milestones the resource API actually persisted. In particular,
 * a poll or an expiry deadline is not a provider completion timestamp. */
export function mediaTimeline(job: MediaJob): {
  recorded: MediaMilestone[];
  expiry: string | null;
} {
  const recorded: MediaMilestone[] = [
    {
      label: 'OLP record created',
      at: job.created_at,
      detail:
        'The local media resource was recorded; provider acceptance is a separate outcome.'
    }
  ];
  if (job.last_polled_at)
    recorded.push({
      label: 'Last provider status check',
      at: job.last_polled_at,
      detail:
        'A status check occurred; it does not prove when the provider changed state.'
    });
  if (job.completed_at)
    recorded.push({
      label: 'Terminal state recorded',
      at: job.completed_at,
      detail: `OLP recorded ${job.state}; provider-side completion may have occurred earlier.`
    });
  if (job.updated_at !== job.created_at)
    recorded.push({
      label: 'Last metadata update',
      at: job.updated_at,
      detail: `Current local lifecycle: ${job.lifecycle}.`
    });
  if (job.deleted_at)
    recorded.push({
      label: 'Resource deletion recorded',
      at: job.deleted_at,
      detail: 'Content is no longer available through this resource.'
    });
  recorded.sort((a, b) => a.at.localeCompare(b.at));
  return { recorded, expiry: job.expires_at ?? null };
}
