import { createContext } from 'svelte';
import type { MediaJobFilters } from '$lib/api/media-jobs';
import { MEDIA_JOB_PAGE_SIZE } from '$lib/api/pageSizes';
import { emptyCursorHistory, type CursorHistory } from '$lib/api/pagination';
import { instant } from '$lib/api/query';

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
    applied: { limit: MEDIA_JOB_PAGE_SIZE }
  };
}

export function mediaJobFilters(
  state: MediaJobListState
): Omit<MediaJobFilters, 'cursor'> {
  return {
    limit: MEDIA_JOB_PAGE_SIZE,
    route: state.route.trim() || undefined,
    state: state.jobState || undefined,
    lifecycle: state.lifecycle || undefined,
    api_key_id: state.apiKeyId.trim() || undefined,
    provider_id: state.providerId.trim() || undefined,
    created_after: instant(state.createdAfter),
    created_before: instant(state.createdBefore)
  };
}
