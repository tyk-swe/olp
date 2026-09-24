"""Pinned official OpenAI Python client through a certified strict video fixture."""

import os
import re

from openai import OpenAI


with OpenAI(base_url=f"{os.environ['OLP_VIDEO_BASE']}/v1", api_key=os.environ['OLP_VIDEO_KEY'], max_retries=0, timeout=15.0) as client:
    created = client.videos.create(
        prompt='one frame, eight seconds',
        input_reference=('first-frame.png', bytes([0x89, 0x50, 0x4e, 0x47, 0, 1, 0xff]), 'image/png'),
        model=os.environ['OLP_VIDEO_ROUTE'],
        seconds='8',
        size='1280x720',
    )
    assert re.fullmatch(r'[a-f0-9-]{36}', created.id)
    assert created.status == 'queued' and created.model == os.environ['OLP_VIDEO_ROUTE']
    completed = client.videos.retrieve(created.id)
    assert completed.id == created.id and completed.status == 'completed'
    video = client.videos.download_content(created.id)
    assert video.content == bytes([0, 0, 0, 8, 109, 111, 111, 118])
    assert video.response.headers['content-type'] == 'video/mp4; codecs="avc1.42E01E"'
    thumbnail = client.videos.download_content(created.id, variant='thumbnail')
    assert thumbnail.content == bytes([0xff, 0xd8, 0, 1, 0xff, 0xd9])
    deleted = client.videos.delete(created.id)
    assert deleted.id == created.id and deleted.deleted is True

print('openai-python-3.8.0: strict video create, retrieve, binary content and delete passed')
