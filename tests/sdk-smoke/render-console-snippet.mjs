// Qualification consumes the production snippet renderer unchanged.
import { readFile } from 'node:fs/promises';
import { sdkSnippet } from '../../console/src/lib/features/access/api-keys/sdkSnippets.ts';
const metadata = JSON.parse(await readFile(process.env.OLP_SDK_SMOKE_METADATA, 'utf8'));
process.stdout.write(sdkSnippet('openai', process.argv.includes('--invalid-key') ? 'olp_not-a-real-key' : metadata.api_key, `${metadata.origin}/`, metadata.route_slug));
