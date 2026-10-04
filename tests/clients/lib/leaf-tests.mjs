// A node:test reporter that lists the tests that ran, one JSON object per line:
// the leaf tests, not the suites that group them and not the stand-in that
// node:test reports for a file that defines none. A group that is skipped or
// left to do is listed as itself, because node:test does not report the tests
// inside a skipped group: the group is all there is to refuse. run.sh passes
// the list to lib/check-ran.sh, which refuses a suite that ran nothing, a test
// file that defines nothing, and any test or group that was skipped or left to
// do.
import path from 'node:path';

/**
 * The record of a finished test, or of a group that is skipped or left to do,
 * and undefined for what is not a test that ran: any other suite, or the file
 * node:test counts as one test, named after the path it was given, when it
 * defines none.
 */
export function leafTest(event) {
  if (event.type !== 'test:pass' && event.type !== 'test:fail') return undefined;
  const { name, nesting, file, skip, todo, details } = event.data;
  const base = path.basename(file ?? '');
  if (details?.type === 'suite') {
    if (!skip && !todo) return undefined;
  } else if (details?.type !== 'test' || (nesting === 0 && path.basename(name) === base)) {
    return undefined;
  }
  return { file: base, name, skip: Boolean(skip), todo: Boolean(todo), failed: event.type === 'test:fail' };
}

export default async function* (source) {
  for await (const event of source) {
    const test = leafTest(event);
    if (test) yield `${JSON.stringify(test)}\n`;
  }
}
