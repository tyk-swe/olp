import { readdirSync } from 'node:fs';
import { dirname, join, relative, sep } from 'node:path';

const sourceExtension = /\.(?:js|mjs|cjs|ts|tsx|svelte)$/;

function directories(path) {
  return readdirSync(path, { withFileTypes: true })
    .filter((entry) => entry.isDirectory())
    .map((entry) => entry.name)
    .sort();
}

function sourceFiles(path) {
  return readdirSync(path, { withFileTypes: true }).flatMap((entry) => {
    const child = join(path, entry.name);
    if (entry.isDirectory()) return sourceFiles(child);
    return entry.isFile() && sourceExtension.test(entry.name) ? [child] : [];
  });
}

function importPatterns(file, featureRoot, restrictedRoots) {
  return restrictedRoots.flatMap((root) => {
    const featurePath = relative(featureRoot, root).split(sep).join('/');
    let relativePath = relative(dirname(file), root).split(sep).join('/');
    if (!relativePath.startsWith('.')) relativePath = `./${relativePath}`;
    return [
      `$lib/features/${featurePath}`,
      `$lib/features/${featurePath}/**`,
      relativePath,
      `${relativePath}/**`
    ];
  });
}

export function buildFeatureBoundaryConfigs(featureRoot, configRoot) {
  const areas = directories(featureRoot);
  return areas.flatMap((area) => {
    const areaRoot = join(featureRoot, area);
    const slices = directories(areaRoot);
    const otherAreas = areas.filter((candidate) => candidate !== area);

    return sourceFiles(areaRoot).map((file) => {
      const withinArea = relative(areaRoot, file).split(sep);
      const slice = slices.includes(withinArea[0]) ? withinArea[0] : null;
      const restrictedRoots = [
        ...otherAreas.map((candidate) => join(featureRoot, candidate)),
        ...(slice
          ? slices
              .filter((candidate) => candidate !== slice)
              .map((candidate) => join(areaRoot, candidate))
          : [])
      ];
      const group = importPatterns(file, featureRoot, restrictedRoots);

      return {
        files: [relative(configRoot, file).split(sep).join('/')],
        rules: group.length
          ? {
              'no-restricted-imports': [
                'error',
                {
                  patterns: [
                    {
                      group,
                      message:
                        'Feature areas are isolated; child slices may use their own slice and area-level shared modules only.'
                    }
                  ]
                }
              ]
            }
          : {}
      };
    });
  });
}
