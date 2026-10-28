import { readFileSync } from 'node:fs';

const summaryFields = [
  'test_efficacy',
  'mutations_coverage',
  'mutants_total',
  'mutants_killed',
  'mutants_lived',
  'mutants_not_covered',
  'mutants_not_viable'
];

export function killableMutants(report) {
  for (const field of summaryFields) {
    if (typeof report[field] !== 'number')
      throw new Error(`mutation report is missing numeric field ${field}`);
  }
  return report.mutants_killed + report.mutants_lived + report.mutants_not_covered;
}

export function assessMutation(report, { efficacy, mcover }) {
  if (!Number.isFinite(efficacy) || !Number.isFinite(mcover))
    throw new Error('mutation thresholds must be finite numbers');
  if (killableMutants(report) === 0) return [];
  return [
    ...(report.test_efficacy < efficacy
      ? [`efficacy ${report.test_efficacy.toFixed(2)}% is below ${efficacy}%`]
      : []),
    ...(report.mutations_coverage < mcover
      ? [`mutant coverage ${report.mutations_coverage.toFixed(2)}% is below ${mcover}%`]
      : [])
  ];
}

if (import.meta.main) {
  const args = process.argv.slice(2);
  const option = (name) => {
    const index = args.indexOf(name);
    const value = Number(args[index + 1]);
    if (index < 0 || args[index + 1] === undefined || !Number.isFinite(value))
      throw new Error(`usage: check-mutation.mjs --efficacy N --mcover N report.json...`);
    return value;
  };
  const thresholds = { efficacy: option('--efficacy'), mcover: option('--mcover') };
  const files = args.filter((arg, i) => !arg.startsWith('--') && !args[i - 1]?.startsWith('--'));
  if (files.length === 0) throw new Error('no mutation report files given');
  let failed = false;
  for (const file of files) {
    const report = JSON.parse(readFileSync(file, 'utf8'));
    if (killableMutants(report) === 0)
      console.error(`check-mutation: ${file}: no killable mutants, nothing assessed`);
    for (const violation of assessMutation(report, thresholds)) {
      console.error(`check-mutation: ${file}: ${violation}`);
      failed = true;
    }
  }
  if (failed) process.exit(1);
  console.log(`check-mutation: ${files.length} report(s) within thresholds`);
}
