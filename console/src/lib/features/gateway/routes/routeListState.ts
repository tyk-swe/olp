import {
  emptyCursorHistory,
  listState,
  type CursorHistory
} from '$lib/api/pagination';

export type RouteListState = {
  draft: CursorHistory;
  route: CursorHistory;
};

export const routeList = listState<RouteListState>(() => ({
  draft: emptyCursorHistory(),
  route: emptyCursorHistory()
}));

export const {
  get: getRouteListState,
  set: setRouteListState,
  empty: emptyRouteListState
} = routeList;
