import type { RequestFilters } from '$lib/api/operations';

export const requestListStateContext = Symbol('request-list-state');

export type RequestListState = {
  route: string;
  providerId: string;
  model: string;
  apiKeyId: string;
  operation: string;
  statusCode: string;
  errorClass: string;
  startedAfter: string;
  startedBefore: string;
  applied: RequestFilters;
  cursors: string[];
};
