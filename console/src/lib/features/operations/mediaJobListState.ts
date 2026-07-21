import type { MediaJobFilters } from '$lib/api/operations';

export const mediaJobListStateContext = Symbol('media-job-list-state');

export type MediaJobListState = {
  route: string;
  jobState: string;
  lifecycle: string;
  cursor: string | undefined;
  history: Array<string | undefined>;
  applied: MediaJobFilters;
};
