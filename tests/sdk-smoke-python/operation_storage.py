"""Exercise the pinned client parser against the real strict gateway surface."""
import base64
import os
from typing import Any
from openai import OpenAI, BadRequestError

base = os.environ['OLP_OPERATION_BASE']
route = os.environ['OLP_OPERATION_FLOAT_ROUTE']
packed_route = os.environ['OLP_OPERATION_PACKED_ROUTE']
client = OpenAI(base_url=base + '/v1', api_key=os.environ['OLP_OPERATION_FLOAT_KEY'], max_retries=0)
implicit = client.embeddings.create(model=route, input='native SDK input')
assert implicit.data[0].embedding == [1, -2]
explicit = client.embeddings.create(model=route, input='native SDK input', encoding_format='base64')
assert explicit.data[0].embedding == 'AACAPwAAAMA='
raw = OpenAI(base_url=base, api_key=os.environ['OLP_OPERATION_PACKED_KEY'], max_retries=0)
path = '/native/voyage-embeddings/models/' + packed_route
body = dict(model=packed_route, input='native bytes', output_dtype='uint8', encoding_format='base64', input_type=None)
try:
    raw.post(path, cast_to=dict[str, Any], body=body)
    raise AssertionError('Missing raw client contract was admitted')
except BadRequestError as error:
    assert error.code == 'state_carrier'
native = raw.post(path, cast_to=dict[str, Any], body=body, options={'headers': {'X-OLP-Client-Contract': 'raw-vector-storage/1'}})
assert native['data'][0]['embedding'] == 'AP8='
assert list(base64.b64decode(native['data'][0]['embedding'])) == [0, 255]
unsafe = OpenAI(base_url=base + '/v1', api_key=os.environ['OLP_OPERATION_PACKED_KEY'], max_retries=0)
try:
    unsafe.embeddings.create(model=packed_route, input='no dtype proof from SDK encoding')
    raise AssertionError('SDK-injected encoding established a false vector contract')
except BadRequestError as error:
    assert error.code == 'target_capability'
print('Pinned Python operation storage contracts passed.')
