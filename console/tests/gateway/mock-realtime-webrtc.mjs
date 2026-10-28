import { createHash } from 'node:crypto';
import { createServer } from 'node:http';

const host = '127.0.0.1';
const port = 4191;
const origin = `http://${host}:${port}`;
const model = 'fixture-voice';
const secret = 'webrtc-fixture-secret';
const answerSDP = 'v=0\r\no=- 3 4 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\n';
const state = { calls: [], hangups: 0, controls: 0, unexpected: [] };

function answer(response, status, body, type = 'application/json') {
  response.writeHead(status, {
    'content-type': type,
    'cache-control': 'no-store'
  });
  response.end(body);
}

async function read(request) {
  const chunks = [];
  let bytes = 0;
  for await (const chunk of request) {
    bytes += chunk.length;
    if (bytes > 1 << 20) throw new Error('body limit');
    chunks.push(chunk);
  }
  return Buffer.concat(chunks).toString('utf8');
}

function frame(opcode, payload) {
  const data = Buffer.from(payload);
  const header =
    data.length < 126
      ? Buffer.from([0x80 | opcode, data.length])
      : Buffer.from([
          0x80 | opcode,
          126,
          (data.length >> 8) & 0xff,
          data.length & 0xff
        ]);
  return Buffer.concat([header, data]);
}

const server = createServer(async (request, response) => {
  const path = new URL(request.url ?? '/', origin).pathname;
  if (request.method === 'GET' && path === '/health')
    return answer(response, 200, '{"status":"ok"}');
  if (request.method === 'POST' && path === '/__test__/reset') {
    state.calls.length = 0;
    state.hangups = 0;
    state.controls = 0;
    state.unexpected.length = 0;
    response.writeHead(204, { 'cache-control': 'no-store' });
    return response.end();
  }
  if (request.method === 'GET' && path === '/__test__/requests')
    return answer(response, 200, JSON.stringify(state));
  if (request.headers.authorization !== `Bearer ${secret}`) {
    state.unexpected.push(`credential ${request.method} ${path}`);
    return answer(response, 401, '{"error":{"message":"bad credential"}}');
  }
  if (request.method === 'GET' && path === '/v1/models')
    return answer(
      response,
      200,
      JSON.stringify({ data: [{ id: model, object: 'model' }] })
    );
  if (request.method === 'POST' && path === '/v1/realtime/calls') {
    const body = await read(request);
    state.calls.push(body.includes(model) ? 'bound' : 'unbound');
    response.writeHead(201, {
      'content-type': 'application/sdp',
      location: '/v1/realtime/calls/rtc_fixture',
      'cache-control': 'no-store'
    });
    return response.end(answerSDP);
  }
  if (
    request.method === 'POST' &&
    path === '/v1/realtime/calls/rtc_fixture/hangup'
  ) {
    state.hangups += 1;
    return answer(response, 200, '{}');
  }
  state.unexpected.push(`${request.method} ${path}`);
  return answer(
    response,
    404,
    '{"error":{"message":"unregistered fixture path"}}'
  );
});

server.on('upgrade', (request, socket) => {
  const url = new URL(request.url ?? '/', origin);
  if (
    url.pathname !== '/v1/realtime' ||
    request.headers.authorization !== `Bearer ${secret}`
  ) {
    state.unexpected.push(`upgrade ${url.pathname}`);
    socket.destroy();
    return;
  }
  const accept = createHash('sha1')
    .update(
      `${request.headers['sec-websocket-key']}258EAFA5-E914-47DA-95CA-C5AB0DC85B11`
    )
    .digest('base64');
  socket.write(
    'HTTP/1.1 101 Switching Protocols\r\n' +
      'Upgrade: websocket\r\n' +
      'Connection: Upgrade\r\n' +
      `Sec-WebSocket-Accept: ${accept}\r\n\r\n`
  );
  state.controls += 1;
  socket.write(
    frame(
      1,
      JSON.stringify({
        type: 'response.done',
        response: {
          id: 'resp_fixture',
          usage: { input_tokens: 3, output_tokens: 2 }
        }
      })
    )
  );
  let pending = Buffer.alloc(0);
  socket.on('data', (chunk) => {
    pending = Buffer.concat([pending, chunk]);
    while (pending.length >= 2) {
      const opcode = pending[0] & 0x0f;
      const masked = (pending[1] & 0x80) !== 0;
      let length = pending[1] & 0x7f;
      let offset = 2;
      if (length === 126) {
        if (pending.length < 4) return;
        length = pending.readUInt16BE(2);
        offset = 4;
      } else if (length === 127) {
        socket.destroy();
        return;
      }
      const maskBytes = masked ? 4 : 0;
      if (pending.length < offset + maskBytes + length) return;
      const payload = Buffer.from(
        pending.subarray(offset + maskBytes, offset + maskBytes + length)
      );
      if (masked) {
        const mask = pending.subarray(offset, offset + 4);
        for (let i = 0; i < payload.length; i++) payload[i] ^= mask[i % 4];
      }
      pending = pending.subarray(offset + maskBytes + length);
      if (opcode === 9) socket.write(frame(10, payload));
      if (opcode === 8) socket.end(frame(8, Buffer.alloc(0)));
    }
  });
  socket.on('error', () => socket.destroy());
});

server.listen(port, host);
