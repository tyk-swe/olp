import type { CursorHistory } from '$lib/api/pagination';

export const routeListStateContext = Symbol('route-list-state');

export type RouteListState = {
  draft: CursorHistory;
  route: CursorHistory;
};
