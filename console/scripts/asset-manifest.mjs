import { createHash } from 'node:crypto';
import { readdir, readFile, writeFile } from 'node:fs/promises';
import { join, relative } from 'node:path';

const root = 'build';
const files = [];
async function collect(directory) {
  for (const entry of await readdir(directory, { withFileTypes: true })) {
    const path = join(directory, entry.name);
    if (entry.isSymbolicLink())
      throw new Error('Console assets must not be symlinks');
    if (entry.isDirectory()) await collect(path);
    else if (entry.isFile() && path !== join(root, 'asset-manifest.json')) {
      files.push({
        path: relative(root, path).split('\\').join('/'),
        sha256: createHash('sha256')
          .update(await readFile(path))
          .digest('hex')
      });
    }
  }
}
await collect(root);
files.sort((a, b) => a.path.localeCompare(b.path, 'en'));
if (!files.some((file) => file.path === 'index.html'))
  throw new Error('Console entry point is missing');
await writeFile(
  join(root, 'asset-manifest.json'),
  JSON.stringify({ version: 1, files }) + '\n'
);
console.log(`Verified console manifest: ${files.length} assets`);
