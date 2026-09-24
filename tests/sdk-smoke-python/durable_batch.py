"""Pinned official OpenAI Python client through strict durable resources."""

import json
import os

from openai import OpenAI

origin = os.environ['OLP_DURABLE_BASE']
route = os.environ['OLP_DURABLE_ROUTE']
model = os.environ['OLP_DURABLE_MODEL']
key = os.environ['OLP_DURABLE_KEY']
input_lines = [
    dict(custom_id='first', method='POST', url='/v1/embeddings', body=dict(model=model, input='alpha')),
    dict(custom_id='second', method='POST', url='/v1/embeddings', body=dict(model=model, input='beta')),
]
payload = ('\n'.join(json.dumps(line, separators=(',', ':')) for line in input_lines) + '\n').encode()

with OpenAI(base_url=f'{origin}/v1', api_key=key, max_retries=0, timeout=15.0) as client:
    uploaded = client.files.create(file=('input.jsonl', payload, 'application/jsonl'), purpose='batch', extra_headers={'X-OLP-Route': route})
    assert uploaded.id.startswith('strict_file_')
    created = client.batches.create(input_file_id=uploaded.id, endpoint='/v1/embeddings', completion_window='24h')
    assert created.id.startswith('strict_batch_') and created.input_file_id == uploaded.id
    cancelled = client.batches.cancel(created.id)
    assert cancelled.status == 'cancelling'
    finished = client.batches.retrieve(created.id)
    assert finished.status == 'completed'
    assert finished.request_counts.completed == 1 and finished.request_counts.failed == 1
    output = client.files.content(finished.output_file_id)
    errors = client.files.content(finished.error_file_id)
    assert b'"custom_id":"first"' in output.content
    assert b'"custom_id":"second"' in errors.content

print('openai-python-3.8.0: strict durable batch lifecycle and partial files passed')
