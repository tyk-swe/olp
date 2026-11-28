import { existsSync, readFileSync } from 'node:fs';

const listed = Object.keys(
  JSON.parse(readFileSync('eslint-suppressions.json', 'utf8'))
);
const gone = listed.filter((file) => !existsSync(file));

if (gone.length > 0) {
  console.error(
    `eslint-suppressions.json lists files that no longer exist. Run pnpm --dir console exec eslint . --prune-suppressions\n${gone.join('\n')}`
  );
  process.exitCode = 1;
}
