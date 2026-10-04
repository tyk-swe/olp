// A recording proxy between a client and the gateway: what the client actually
// sent and what OLP answered, byte for byte. The scripted upstream records the
// other end of the same exchange, so a suite can hold the two against each
// other: a gateway that mangles a request, or a client that does not send what
// it claims, shows up as a difference.
//
// Point a client at `tap.baseURLs` instead of `baseURLs`. It is a pure relay:
// nothing is rewritten in either direction, and a client that ignores its base
// URL reaches nothing, because the run's proxy refuses everything but
// loopback.
import assert from 'node:assert/strict';
import http from 'node:http';
import { setTimeout as sleep } from 'node:timers/promises';
import { origin as gatewayOrigin } from './harness.mjs';

const hopByHop = new Set(['connection', 'keep-alive', 'transfer-encoding', 'proxy-connection', 'upgrade', 'te', 'trailer']);
// A relayed response is kept for assertions up to this size; the relay itself
// never truncates.
const keepBytes = 1 << 20;

/**
 * Start a tap in front of the gateway. Returns the base URLs for each surface,
 * `sent()` for the exchanges so far, `reset()` and `close()`.
 */
export async function startTap() {
  const target = new URL(gatewayOrigin);
  const exchanges = [];
  let inFlight = 0;
  const agent = new http.Agent({ keepAlive: false });

  const server = http.createServer((request, response) => {
    inFlight++;
    const url = new URL(request.url, 'http://tap');
    const exchange = {
      method: request.method,
      path: url.pathname,
      query: url.search.replace(/^\?/, ''),
      headers: { ...request.headers },
      bodyText: '',
      status: 0,
      responseHeaders: {},
      responseText: '',
      error: undefined
    };
    // Exchanges are listed in the order the requests arrived, which is the
    // order the upstream recorded them in, however they finish.
    exchanges.push(exchange);
    const requestChunks = [];
    request.on('data', (chunk) => requestChunks.push(chunk));

    // The relay can end more than once, for instance when a client gives up
    // mid-stream and the destroyed request errors as well.
    let finished = false;
    const finish = () => {
      if (finished) return;
      finished = true;
      exchange.bodyText = Buffer.concat(requestChunks).toString('utf8');
      exchange.body = parseJSON(exchange.bodyText);
      inFlight--;
    };

    const relayed = http.request(
      {
        host: target.hostname,
        port: target.port,
        method: request.method,
        path: request.url,
        headers: { ...request.headers, host: target.host },
        agent
      },
      (answer) => {
        exchange.status = answer.statusCode;
        exchange.responseHeaders = { ...answer.headers };
        const headers = Object.fromEntries(Object.entries(answer.headers).filter(([name]) => !hopByHop.has(name)));
        response.writeHead(answer.statusCode, headers);
        const answerChunks = [];
        let kept = 0;
        answer.on('data', (chunk) => {
          if (kept < keepBytes) answerChunks.push(chunk);
          kept += chunk.length;
          response.write(chunk);
        });
        answer.on('end', () => {
          exchange.responseText = Buffer.concat(answerChunks).toString('utf8');
          response.end();
          finish();
        });
        answer.on('error', (error) => {
          exchange.error = String(error);
          response.destroy(error);
          finish();
        });
      }
    );
    relayed.on('error', (error) => {
      exchange.error = String(error);
      exchange.status = 502;
      if (!response.headersSent) response.writeHead(502);
      response.end();
      finish();
    });
    // A client that gives up mid-stream ends the relay too.
    response.on('close', () => {
      if (!response.writableEnded) relayed.destroy();
    });
    request.pipe(relayed);
  });

  await new Promise((resolve, reject) => server.once('error', reject).listen(0, '127.0.0.1', resolve));
  const origin = `http://127.0.0.1:${server.address().port}`;

  return {
    origin,
    /** Base URLs as {@link baseURLs} in harness.mjs gives them, through the tap. */
    baseURLs: { openai: `${origin}/v1`, anthropic: `${origin}/anthropic`, gemini: `${origin}/gemini` },
    /**
     * Every exchange so far in arrival order. It waits for exchanges still in
     * flight, so a stream the client has finished reading is complete.
     */
    async sent() {
      for (let attempt = 0; attempt < 100; attempt++) {
        if (inFlight === 0) return [...exchanges];
        await sleep(20);
      }
      assert.fail('client requests are still in flight');
    },
    reset() {
      exchanges.length = 0;
    },
    async close() {
      await new Promise((resolve) => {
        server.close(resolve);
        server.closeAllConnections();
      });
      agent.destroy();
    }
  };
}

function parseJSON(text) {
  try {
    return text ? JSON.parse(text) : undefined;
  } catch {
    return undefined;
  }
}

/** The data of every server-sent event of a body, as parsed JSON. */
export function sseData(text) {
  return text
    .replaceAll('\r\n', '\n')
    .split('\n\n')
    .map((frame) => frame.split('\n').find((line) => line.startsWith('data:')))
    .filter(Boolean)
    .map((line) => line.slice('data:'.length).trim())
    .filter((data) => data !== '[DONE]')
    .map((data) => JSON.parse(data));
}
