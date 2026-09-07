import type { MediaJobFilters } from '$lib/api/media-jobs';
import { MEDIA_JOB_PAGE_SIZE } from '$lib/api/pageSizes';
import {
  filteredListState,
  type FilteredListState
} from '$lib/lists/pagination';
import {
  formTimeValue,
  preservedTime,
  querySearch,
  rangeProblem,
  readForm,
  timeProblem,
  uuidProblem
} from '$lib/lists/filters';
import type { ListUrlSpec } from '$lib/lists/urlSync.svelte';

export type MediaJobForm = {
  route: string;
  jobState: string;
  lifecycle: string;
  apiKeyId: string;
  providerId: string;
  createdAfter: string;
  createdBefore: string;
};

export type MediaJobQuery = Omit<MediaJobFilters, 'cursor'>;
export type MediaJobListState = FilteredListState<MediaJobForm, MediaJobQuery>;

const params = {
  route: 'route',
  jobState: 'state',
  lifecycle: 'lifecycle',
  apiKeyId: 'api_key_id',
  providerId: 'provider_id',
  createdAfter: 'created_after',
  createdBefore: 'created_before'
} as const;

export function mediaJobFilters(
  state: MediaJobForm,
  applied?: MediaJobQuery
): MediaJobQuery {
  return {
    limit: MEDIA_JOB_PAGE_SIZE,
    route: state.route.trim() || undefined,
    state: state.jobState || undefined,
    lifecycle: state.lifecycle || undefined,
    api_key_id: state.apiKeyId.trim() || undefined,
    provider_id: state.providerId.trim() || undefined,
    created_after: preservedTime(state.createdAfter, applied?.created_after),
    created_before: preservedTime(state.createdBefore, applied?.created_before)
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

export const mediaJobStates = [
  'queued',
  'running',
  'succeeded',
  'failed',
  'cancelled'
];
export const mediaJobLifecycles = [
  'creating',
  'active',
  'create_ambiguous',
  'create_cleanup_pending',
  'delete_pending',
  'deleted'
];

export function mediaJobProblem(
  form: MediaJobForm,
  fromUrl = false,
  applied?: MediaJobQuery
): string | null {
  const ids = uuidProblem([
    [form.providerId, 'Provider ID'],
    [form.apiKeyId, 'API key ID']
  ]);
  if (ids) return ids;
  if (form.jobState && !mediaJobStates.includes(form.jobState))
    return `Unknown media-job state: ${form.jobState}`;
  if (form.lifecycle && !mediaJobLifecycles.includes(form.lifecycle))
    return `Unknown media-job lifecycle: ${form.lifecycle}`;
  const times = timeProblem(
    [
      [form.createdAfter, 'Created after'],
      [form.createdBefore, 'Created before']
    ],
    fromUrl
  );
  if (times) return times;
  const query = mediaJobFilters(form, applied);
  return rangeProblem(
    query.created_after,
    query.created_before,
    'Created before must be later than created after.'
  );
}

export function readMediaJobForm(search: URLSearchParams): MediaJobForm {
  return readForm<MediaJobForm>(search, params);
}

export function mediaJobState(search: URLSearchParams): MediaJobListState {
  const form = readMediaJobForm(search);
  return {
    ...mediaJobList.empty(),
    ...form,
    applied: mediaJobFilters(form),
    createdAfter: formTimeValue(form.createdAfter),
    createdBefore: formTimeValue(form.createdBefore)
  };
}

export function mediaJobSearch(applied: MediaJobQuery): string {
  return querySearch(applied, Object.values(params));
}

export const mediaJobUrl: ListUrlSpec<MediaJobListState> = {
  path: '/media-jobs',
  state: mediaJobState,
  canonical: (search) => {
    const form = readMediaJobForm(search);
    return mediaJobProblem(form, true)
      ? null
      : mediaJobSearch(mediaJobFilters(form));
  }
};
