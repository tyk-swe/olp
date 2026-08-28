import type { MediaJobFilters } from '$lib/api/media-jobs';
import { MEDIA_JOB_PAGE_SIZE } from '$lib/api/pageSizes';
import { filteredListState, type FilteredListState } from '$lib/api/pagination';
import { instant } from '$lib/api/query';

export type MediaJobForm = {
  route: string;
  jobState: string;
  lifecycle: string;
  apiKeyId: string;
  providerId: string;
  createdAfter: string;
  createdBefore: string;
};

export type MediaJobListState = FilteredListState<
  MediaJobForm,
  Omit<MediaJobFilters, 'cursor'>
>;

export function mediaJobFilters(
  state: MediaJobForm
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

export const mediaJobList = filteredListState({
  emptyForm: (): MediaJobForm => ({
    route: '',
    jobState: '',
    lifecycle: '',
    apiKeyId: '',
    providerId: '',
    createdAfter: '',
    createdBefore: ''
  }),
  toQuery: mediaJobFilters
});

export const {
  get: getMediaJobListState,
  set: setMediaJobListState,
  empty: emptyMediaJobListState
} = mediaJobList;
