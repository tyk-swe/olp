import {
  emptyCursorHistory,
  listState,
  type CursorHistory
} from '$lib/listing/pagination';

export type RouteListState = {
  draft: CursorHistory;
  route: CursorHistory;
};

export const routeList = listState<RouteListState>(() => ({
  draft: emptyCursorHistory(),
  route: emptyCursorHistory()
}));
