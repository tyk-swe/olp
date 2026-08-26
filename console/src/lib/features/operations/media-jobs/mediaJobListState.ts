import { createContext } from 'svelte';
import type { MediaJobFilters } from '$lib/api/media-jobs';
import { emptyCursorHistory, type CursorHistory } from '$lib/api/pagination';

export type MediaJobListState = CursorHistory & {
  route: string;
  jobState: string;
  lifecycle: string;
  apiKeyId: string;
  providerId: string;
  createdAfter: string;
  createdBefore: string;
  applied: Omit<MediaJobFilters, 'cursor'>;
};

export const [getMediaJobListState, setMediaJobListState] =
  createContext<MediaJobListState>();

export function emptyMediaJobListState(): MediaJobListState {
  return {
    ...emptyCursorHistory(),
    route: '',
    jobState: '',
    lifecycle: '',
    apiKeyId: '',
    providerId: '',
    createdAfter: '',
    createdBefore: '',
    applied: { limit: 25 }
  };
}

/**
 * The date inputs are local-time `datetime-local` values; the API compares
 * instants. A half-typed date stays out of the query rather than being sent as
 * an invalid bound the backend would reject.
 */
function instant(value: string): string | undefined {
  if (!value.trim()) return undefined;
  const date = new Date(value);
  return Number.isNaN(date.valueOf()) ? undefined : date.toISOString();
}

export function mediaJobFilters(
  state: MediaJobListState
): Omit<MediaJobFilters, 'cursor'> {
  return {
    limit: 25,
    route: state.route.trim() || undefined,
    state: state.jobState || undefined,
    lifecycle: state.lifecycle || undefined,
    api_key_id: state.apiKeyId.trim() || undefined,
    provider_id: state.providerId.trim() || undefined,
    created_after: instant(state.createdAfter),
    created_before: instant(state.createdBefore)
  };
}
