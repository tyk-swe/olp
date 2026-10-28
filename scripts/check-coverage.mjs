import { readFileSync } from 'node:fs';

export function parseCoverage(output) {
  return new Map(
    output
      .split('\n')
      .flatMap((line) => {
        const match = /^(?:ok\s+)?\s*(\S+)\s+(?:\S+\s+)?coverage: ([\d.]+)%/.exec(line);
        return match ? [[match[1], Number(match[2])]] : [];
      })
  );
}

export function compareCoverage(base, head) {
  return [...base].flatMap(([pkg, before]) => {
    const after = head.get(pkg);
    if (after === undefined) return [`${pkg} is missing at head (base coverage ${before}%)`];
    if (after < before)
      return [`${pkg} coverage dropped from ${before}% to ${after}%`];
    return [];
  });
}

if (import.meta.main) {
  const [baseFile, headFile] = process.argv.slice(2);
  if (!baseFile || !headFile)
    throw new Error('usage: check-coverage.mjs base.txt head.txt');
  const violations = compareCoverage(
    parseCoverage(readFileSync(baseFile, 'utf8')),
    parseCoverage(readFileSync(headFile, 'utf8'))
  );
  for (const violation of violations) console.error(`check-coverage: ${violation}`);
  if (violations.length > 0) process.exit(1);
  console.log('check-coverage: no changed package lost coverage');
}
