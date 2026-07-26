import type { MediaJobFilters } from '$lib/api/operations';
import type { CursorHistory } from '$lib/api/pagination';

export const mediaJobListStateContext = Symbol('media-job-list-state');

export type MediaJobListState = CursorHistory & {
  route: string;
  jobState: string;
  lifecycle: string;
  applied: MediaJobFilters;
};
