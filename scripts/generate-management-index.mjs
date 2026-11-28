import { readdirSync, readFileSync, writeFileSync } from 'node:fs';
import { join, resolve } from 'node:path';

const namedSections = ['paths', 'webhooks'];
const methods = ['get', 'put', 'post', 'delete', 'options', 'head', 'patch', 'trace'];
const componentReference = /^(?:\.\/shared\.json)?#\/components\/[^/]+\/[^/]+$/;

const parse = ({ file, path }) => {
  const text = readFileSync(path, 'utf8');
  try {
    const body = JSON.parse(text);
    return `${JSON.stringify(body, null, 2)}\n` === text
      ? { file, path, body }
      : {
          file,
          problem: `${file} is not canonical two-space JSON, which a key repeated inside one object also causes`
        };
  } catch (error) {
    return { file, problem: `${file} is not valid JSON: ${error.message}` };
  }
};

const entriesOf = ({ file, path, body }) =>
  Object.entries(body).flatMap(([section, value]) => {
    const entry = (at, node) => ({ file, path, at, node });
    if (namedSections.includes(section))
      return Object.entries(value).map(([name, node]) => entry([section, name], node));
    if (section === 'components')
      return Object.entries(value).flatMap(([kind, members]) =>
        Object.entries(members).map(([name, node]) => entry([section, kind, name], node))
      );
    return [entry([section], value)];
  });

const operationsOf = ({ file, at: [section, name], node }) =>
  namedSections.includes(section)
    ? methods
        .filter((method) => node[method])
        .map((method) => ({ file, label: `${method.toUpperCase()} ${name}`, ...node[method] }))
    : [];

const referencesIn = (node) => {
  if (!node || typeof node !== 'object') return [];
  return Object.entries(node).flatMap(([key, child]) =>
    key === '$ref' && typeof child === 'string' ? [child] : referencesIn(child)
  );
};

const definedTwice = (items, identify) =>
  [...Map.groupBy(items, identify)].filter(([, group]) => group.length > 1);

function problemsIn(fragments, entries) {
  const operations = entries.flatMap(operationsOf);
  return [
    ...definedTwice(entries, ({ at }) => at.join(' ')).map(
      ([name, group]) => `${name} is defined in ${group.map(({ file }) => file).join(' and ')}`
    ),
    ...definedTwice(
      operations.filter(({ operationId }) => operationId),
      ({ operationId }) => operationId
    ).map(
      ([id, group]) =>
        `operationId ${id} is used by ${group.map(({ label, file }) => `${label} in ${file}`).join(' and ')}`
    ),
    ...operations.flatMap(({ file, label, tags: [tag] = [] }) => {
      const home = tag && `${tag.toLowerCase()}.json`;
      return home && home !== file
        ? [`${label} in ${file} is tagged ${tag}, so it belongs in ${home}`]
        : [];
    }),
    ...fragments.flatMap(({ file, body }) =>
      [...new Set(referencesIn(body))]
        .filter((reference) => !componentReference.test(reference))
        .map(
          (reference) =>
            `${file} references ${reference}, but a fragment may only reference a whole component of its own or of ./shared.json, so move a component that two fragments need there`
        )
    )
  ];
}

const pointer = (steps) =>
  steps.map((step) => step.replaceAll('~', '~0').replaceAll('/', '~1')).join('/');

const indexValue = ({ path, at, node }) =>
  at.length > 1 ? { $ref: `${path}#/${pointer(at)}` } : node;

const indexOf = (entries, depth = 0) =>
  Object.fromEntries(
    [...Map.groupBy(entries, ({ at }) => at[depth])]
      .sort(([a], [b]) => (a < b ? -1 : 1))
      .map(([key, group]) => [
        key,
        group[0].at.length === depth + 1 ? indexValue(group[0]) : indexOf(group, depth + 1)
      ])
  );

function managementIndex(directory) {
  const parsed = readdirSync(directory)
    .filter((file) => file.endsWith('.json'))
    .sort()
    .map((file) => parse({ file, path: resolve(join(directory, file)) }));
  const unreadable = parsed.flatMap(({ problem }) => problem ?? []);
  if (unreadable.length) return { problems: unreadable };
  const entries = parsed.flatMap(entriesOf);
  const problems = problemsIn(parsed, entries);
  return problems.length ? { problems } : { problems, index: indexOf(entries) };
}

if (import.meta.main) {
  const [directory, output] = process.argv.slice(2);
  const { problems, index } = managementIndex(directory);
  if (problems.length) {
    console.error(problems.join('\n'));
    process.exitCode = 1;
  } else writeFileSync(output, `${JSON.stringify(index, null, 2)}\n`);
}
