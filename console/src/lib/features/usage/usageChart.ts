import type { UsagePoint } from '#lib/features/usage/usage.js';

export type ChartBounds = {
  readonly width: number;
  readonly height: number;
  readonly left: number;
  readonly right: number;
  readonly top: number;
  readonly bottom: number;
};

export type ChartCoordinate = {
  readonly x: number;
  readonly y: number;
};

export type ChartGeometry = {
  readonly coordinates: readonly ChartCoordinate[];
  readonly polyline: string;
  readonly area: string;
  readonly ticks: readonly { readonly y: number; readonly value: number }[];
};

export function buildUsageChart(
  points: readonly UsagePoint[],
  bounds: ChartBounds
): ChartGeometry {
  const width = bounds.width - bounds.left - bounds.right;
  const height = bounds.height - bounds.top - bounds.bottom;
  const maximum = Math.max(1, ...points.map((point) => point.request_count));
  const step = Math.max(1, Math.ceil(maximum / 4));
  const ceiling = step * 4;
  const times = points.map((point) => Date.parse(point.bucket));
  const first = times[0];
  const span = times.every(Number.isFinite)
    ? (times.at(-1) ?? first) - first
    : Number.NaN;
  const coordinates = points.map((point, index) => ({
    x:
      bounds.left +
      width *
        (span > 0
          ? (times[index] - first) / span
          : index / Math.max(1, points.length - 1)),
    y: bounds.top + height - (point.request_count * height) / ceiling
  }));
  const polyline = coordinates.map(({ x, y }) => `${x},${y}`).join(' ');
  const baseline = bounds.top + height;
  const firstCoordinate = coordinates[0];
  const lastCoordinate = coordinates.at(-1);
  return {
    coordinates,
    polyline,
    area:
      firstCoordinate && lastCoordinate
        ? `${firstCoordinate.x},${baseline} ${polyline} ${lastCoordinate.x},${baseline}`
        : '',
    ticks: Array.from({ length: 5 }, (_, index) => ({
      y: bounds.top + (index * height) / 4,
      value: step * (4 - index)
    }))
  };
}

export function nearestCoordinateIndex(
  coordinates: readonly ChartCoordinate[],
  targetX: number
): number {
  let best = 0;
  let bestDistance = Math.abs(coordinates[0].x - targetX);
  for (let index = 1; index < coordinates.length; index += 1) {
    const distance = Math.abs(coordinates[index].x - targetX);
    if (distance < bestDistance) {
      best = index;
      bestDistance = distance;
    }
  }
  return best;
}

export function stepBucketIndex(
  current: number | null,
  total: number,
  key: string
): { handled: boolean; next: number | null } {
  if (!total) return { handled: false, next: current };
  if (key === 'ArrowRight')
    return {
      handled: true,
      next: current === null ? 0 : Math.min(total - 1, current + 1)
    };
  if (key === 'ArrowLeft')
    return {
      handled: true,
      next: current === null ? total - 1 : Math.max(0, current - 1)
    };
  if (key === 'Home') return { handled: true, next: 0 };
  if (key === 'End') return { handled: true, next: total - 1 };
  if (key === 'Escape') return { handled: true, next: null };
  return { handled: false, next: current };
}
