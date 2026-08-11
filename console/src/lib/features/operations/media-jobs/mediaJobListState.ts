import { createContext } from 'svelte';
import type { MediaJobFilters } from '$lib/api/operations';
import { emptyCursorHistory, type CursorHistory } from '$lib/api/pagination';

export type MediaJobListState = CursorHistory & {
  route: string;
  jobState: string;
  lifecycle: string;
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
    applied: { limit: 25 }
  };
}
