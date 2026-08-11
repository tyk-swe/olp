import { createContext } from 'svelte';
import { emptyCursorHistory, type CursorHistory } from '$lib/api/pagination';

export type RouteListState = {
  draft: CursorHistory;
  route: CursorHistory;
};

export const [getRouteListState, setRouteListState] =
  createContext<RouteListState>();

export function emptyRouteListState(): RouteListState {
  return {
    draft: emptyCursorHistory(),
    route: emptyCursorHistory()
  };
}
