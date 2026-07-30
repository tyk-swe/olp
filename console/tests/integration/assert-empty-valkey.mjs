import net from 'node:net';
import tls from 'node:tls';

const rawUrl = process.env.OLP_VALKEY_URL;
if (!rawUrl) throw new Error('OLP_VALKEY_URL is required');

const url = new URL(rawUrl);
const tcpProtocols = new Set(['redis:', 'rediss:', 'valkey:', 'valkeys:']);
const unixProtocols = new Set(['redis+unix:', 'valkey+unix:', 'unix:']);
const tlsProtocols = new Set(['rediss:', 'valkeys:']);
if (!tcpProtocols.has(url.protocol) && !unixProtocols.has(url.protocol)) {
  throw new Error(
    'OLP_VALKEY_URL must use a URL scheme supported by redis::Client::open'
  );
}

function lastQueryValue(name) {
  return url.searchParams.getAll(name).at(-1);
}

const unixSocket = unixProtocols.has(url.protocol);
const databaseText = unixSocket
  ? (lastQueryValue('db') ?? '0')
  : (url.pathname.replace(/^\/+|\/+$/g, '') || '0');
if (!/^[+-]?\d+$/.test(databaseText)) {
  throw new Error('OLP_VALKEY_URL must select a numeric logical database');
}
const database = BigInt(databaseText);
const minimumI64 = -(1n << 63n);
const maximumI64 = (1n << 63n) - 1n;
if (database < minimumI64 || database > maximumI64) {
  throw new Error('OLP_VALKEY_URL logical database is outside the runtime-supported range');
}

let host;
let port;
let socketPath;
let username;
let password;
let hasPassword;
if (unixSocket) {
  if (url.hostname && url.hostname !== 'localhost') {
    throw new Error('OLP_VALKEY_URL Unix socket host must be empty or localhost');
  }
  socketPath = decodeURIComponent(url.pathname);
  if (socketPath.includes('\0')) {
    throw new Error('OLP_VALKEY_URL Unix socket path contains a null byte');
  }
  username = lastQueryValue('user') ?? '';
  const passwordValue = lastQueryValue('pass');
  password = passwordValue ?? '';
  hasPassword = passwordValue !== undefined;
} else {
  host = url.hostname.startsWith('[') && url.hostname.endsWith(']')
    ? url.hostname.slice(1, -1)
    : url.hostname;
  if (!host) throw new Error('OLP_VALKEY_URL must include a hostname');
  port = Number(url.port || 6379);
  username = decodeURIComponent(url.username);
  password = decodeURIComponent(url.password);
  hasPassword = url.password !== '';
}

function encodeCommand(parts) {
  return Buffer.from(
    `*${parts.length}\r\n${parts.map((part) => `$${Buffer.byteLength(part)}\r\n${part}\r\n`).join('')}`
  );
}

const commands = [];
if (hasPassword) {
  commands.push(username ? ['AUTH', username, password] : ['AUTH', password]);
}
if (database !== 0n) commands.push(['SELECT', String(database)]);
commands.push(['DBSIZE']);

const replies = await new Promise((resolve, reject) => {
  const secure = tlsProtocols.has(url.protocol);
  const socket = unixSocket
    ? net.createConnection({ path: socketPath })
    : secure
    ? tls.connect({
        host,
        port,
        servername: net.isIP(host) ? undefined : host,
        rejectUnauthorized: url.hash !== '#insecure'
      })
    : net.createConnection({ host, port });
  let settled = false;
  let buffer = Buffer.alloc(0);
  const values = [];

  function finish(error, result) {
    if (settled) return;
    settled = true;
    socket.destroy();
    if (error) reject(error);
    else resolve(result);
  }

  socket.setTimeout(5_000);
  socket.on('timeout', () => finish(new Error('Valkey isolation check timed out')));
  socket.on('error', (error) => finish(new Error(`Valkey isolation check failed: ${error.message}`)));
  socket.on(secure ? 'secureConnect' : 'connect', () => {
    socket.write(Buffer.concat(commands.map(encodeCommand)));
  });
  socket.on('data', (chunk) => {
    buffer = Buffer.concat([buffer, chunk]);
    while (buffer.length > 0) {
      const end = buffer.indexOf('\r\n');
      if (end < 0) return;
      const prefix = String.fromCharCode(buffer[0]);
      const value = buffer.subarray(1, end).toString('utf8');
      buffer = buffer.subarray(end + 2);
      if (prefix === '-') {
        finish(new Error(`Valkey rejected the isolation check: ${value}`));
        return;
      }
      if (prefix !== '+' && prefix !== ':') {
        finish(new Error(`Valkey returned unsupported response type ${JSON.stringify(prefix)}`));
        return;
      }
      values.push({ prefix, value });
      if (values.length === commands.length) {
        finish(undefined, values);
        return;
      }
    }
  });
});

const sizeReply = replies.at(-1);
if (sizeReply?.prefix !== ':' || !/^\d+$/.test(sizeReply.value)) {
  throw new Error('Valkey DBSIZE returned an invalid response');
}
const size = BigInt(sizeReply.value);
if (size !== 0n) {
  throw new Error(
    `Valkey logical database ${database} contains ${size} key(s); `
    + 'the console integration suite requires an isolated empty database'
  );
}

console.log(`Valkey logical database ${database} is empty`);
