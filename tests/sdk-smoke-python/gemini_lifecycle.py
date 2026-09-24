import asyncio
import json
import os

from google import genai
from google.genai import types


base = os.environ['OLP_GEMINI_BASE']
key = os.environ['OLP_GEMINI_KEY']
interaction_route = os.environ['OLP_GEMINI_INTERACTION_ROUTE']
live_route = os.environ['OLP_GEMINI_LIVE_ROUTE']
client = genai.Client(
    api_key=key,
    http_options=types.HttpOptions(base_url=base, api_version='v1beta'),
)
first = client.interactions.create(
    model=interaction_route,
    input='hello',
    store=True,
    generation_config={'max_output_tokens': 64, 'seed': 0},
    tools=[{'type': 'function', 'name': 'weather', 'parameters': {'type': 'object'}}],
)
assert first.id.startswith('interaction_')
assert first.steps[0].signature == 'opaque-native'
assert first.steps[1].content[0].text == 'OK'
second = client.interactions.create(
    model=interaction_route,
    input='next',
    previous_interaction_id=first.id,
    store=True,
)
assert second.id.startswith('interaction_') and second.id != first.id
retrieved = client.interactions.get(first.id)
assert retrieved.id == first.id and retrieved.steps[0].content[0].text == 'retrieved'
resumed = list(client.interactions.get(first.id, stream=True, last_event_id='cursor-start'))
assert [event.event_type for event in resumed] == ['step.delta', 'step.stop', 'interaction.completed']
assert resumed[2].interaction.id == first.id
stream = client.interactions.create(model=interaction_route, input='stream', stream=True, store=True)
events = list(stream)
assert [event.event_type for event in events] == [
    'interaction.created', 'step.start', 'step.delta', 'step.stop', 'interaction.completed'
]
assert events[0].interaction.id.startswith('interaction_')
assert events[3].step['signature'] == 'opaque-native'
assert events[4].interaction.id == events[0].interaction.id
cancelled = client.interactions.cancel(first.id)
assert cancelled.id == first.id and cancelled.status == 'cancelled'
client.interactions.delete(first.id)
client.close()


async def live():
    async_client = genai.Client(
        api_key=key,
        http_options=types.HttpOptions(base_url=base, api_version='v1beta'),
    )
    try:
        async with async_client.aio.live.connect(
            model=live_route,
            config={
                'response_modalities': ['AUDIO'],
                'realtime_input_config': {'automatic_activity_detection': {'disabled': True}},
                'session_resumption': {},
            },
        ) as session:
            await session.send_realtime_input(activity_start=types.ActivityStart())
            await session.send_realtime_input(
                audio=types.Blob(mime_type='audio/pcm;rate=16000', data=b'\x01\x02\x03\x04')
            )
            async for response in session.receive():
                if response.server_content:
                    assert response.server_content.interrupted is True
                    assert response.server_content.turn_complete is True
                    assert response.server_content.model_turn.parts[0].inline_data.data == b'\x01\x02\x03\x04'
                    return
            raise AssertionError('Live stream closed before native audio')
    finally:
        await async_client.aio.aclose()


asyncio.run(asyncio.wait_for(live(), timeout=8))
print(json.dumps({'interaction': 'two-turn/stream/get/cancel/delete', 'live': 'audio/interruption', 'versions': 'google-genai==2.22.0'}))
