import { createContext } from 'svelte';
import type { RequestFilters } from '$lib/api/operations';
import { emptyCursorHistory, type CursorHistory } from '$lib/api/pagination';

export type RequestListState = CursorHistory & {
  route: string;
  providerId: string;
  model: string;
  apiKeyId: string;
  operation: string;
  statusCode: string;
  errorClass: string;
  startedAfter: string;
  startedBefore: string;
  applied: Omit<RequestFilters, 'cursor'>;
};

export const [getRequestListState, setRequestListState] =
  createContext<RequestListState>();

export function emptyRequestListState(): RequestListState {
  return {
    ...emptyCursorHistory(),
    route: '',
    providerId: '',
    model: '',
    apiKeyId: '',
    operation: '',
    statusCode: '',
    errorClass: '',
    startedAfter: '',
    startedBefore: '',
    applied: { limit: 25 }
  };
}
