package routing

import "slices"

type RouteWalk struct {
	trail   []*Route
	visited []string
	frames  []routeWalkFrame
}

type routeWalkFrame struct {
	route      *Route
	conditions []string
	next       int
}

func NewRouteWalk(route *Route) RouteWalk {
	return RouteWalk{trail: []*Route{route}, visited: []string{route.Slug}}
}

func (w RouteWalk) Visited(slug string) bool {
	return slices.Contains(w.visited, slug)
}

func (w RouteWalk) Move(route *Route, delegated bool) RouteWalk {
	visited := append(slices.Clone(w.visited), route.Slug)
	if delegated {
		return RouteWalk{trail: append(slices.Clone(w.trail), route), visited: visited, frames: w.frames}
	}
	return RouteWalk{trail: []*Route{route}, visited: visited, frames: w.frames}
}

func (w RouteWalk) Failed(conditions []string) RouteWalk {
	return RouteWalk{trail: w.trail, visited: w.visited, frames: appendWalkFrames(w.frames, w.trail, conditions)}
}

func appendWalkFrames(frames []routeWalkFrame, routes []*Route, conditions []string) []routeWalkFrame {
	if len(routes) == 0 {
		return frames
	}
	route := routes[0]
	if len(route.Fallbacks) == 0 || slices.ContainsFunc(frames, func(frame routeWalkFrame) bool { return frame.route.Slug == route.Slug }) {
		return appendWalkFrames(frames, routes[1:], conditions)
	}
	frame := routeWalkFrame{route: route, conditions: slices.Clone(conditions)}
	return appendWalkFrames(append(slices.Clone(frames), frame), routes[1:], conditions)
}

func (w RouteWalk) Next() (RouteWalk, FallbackStep, bool) {
	if len(w.frames) == 0 {
		return w, FallbackStep{}, false
	}
	top := w.frames[len(w.frames)-1]
	prior := w.frames[:len(w.frames)-1]
	if top.next == len(top.route.Fallbacks) {
		return (RouteWalk{trail: w.trail, visited: w.visited, frames: prior}).Next()
	}
	fallback := top.route.Fallbacks[top.next]
	frame := routeWalkFrame{route: top.route, conditions: top.conditions, next: top.next + 1}
	next := RouteWalk{trail: w.trail, visited: w.visited, frames: append(slices.Clone(prior), frame)}
	met := fallback.Met(top.conditions)
	if len(met) == 0 {
		return next.Next()
	}
	return next, FallbackStep{From: top.route.Slug, Route: fallback.Route, Conditions: met}, true
}

func (w RouteWalk) Pending() []FallbackStep {
	pending := w.Failed(nil)
	return slices.Collect(func(yield func(FallbackStep) bool) {
		for i := len(pending.frames) - 1; i >= 0; i-- {
			frame := pending.frames[i]
			for _, fallback := range frame.route.Fallbacks[frame.next:] {
				if !yield(FallbackStep{From: frame.route.Slug, Route: fallback.Route, Conditions: slices.Clone(fallback.On)}) {
					return
				}
			}
		}
	})
}

func (w RouteWalk) MayLeave() bool {
	return len(w.frames) > 0 || len(w.trail) > 1 || len(w.trail) == 1 && len(w.trail[0].Fallbacks) > 0
}
