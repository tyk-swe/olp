export const routeListStateContext = Symbol('route-list-state');

export type RouteListState = {
  draftCursor: string | undefined;
  draftHistory: Array<string | undefined>;
  routeCursor: string | undefined;
  routeHistory: Array<string | undefined>;
};
