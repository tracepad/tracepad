"""Send a picture through the Langfuse SDK's media channel (spec 041 #9).

The SDK takes the image out of the generation's input itself, asks
`POST /api/public/media` for an upload URL, PUTs the bytes there and reports
back with a PATCH; the span it exports carries its reference string where the
picture was. Two generations in two traces send the same picture, so the
second request must be answered `uploadUrl: null`.

The SDK's own HTTP client is handed a response hook that records what the
media endpoints answered — the one fact the database cannot tell afterwards.
Each generation holds its span open for a moment, as a real model call would:
the upload runs on the SDK's own thread, and a span that arrives before its
picture keeps the reference string as the client wrote it (#9).
"""

import base64
import json
import os
import random
import struct
import sys
import time
import zlib

import httpx
from langfuse import Langfuse

host = os.environ["SMOKE_HOST"]
out_file = sys.argv[1]


def noise_png(side):
    rng = random.Random(41)
    rows = b"".join(b"\x00" + bytes(rng.randrange(256) for _ in range(side * 3))
                    for _ in range(side))

    def chunk(kind, data):
        crc = struct.pack(">I", zlib.crc32(kind + data))
        return struct.pack(">I", len(data)) + kind + data + crc

    header = struct.pack(">IIBBBBB", side, side, 8, 2, 0, 0, 0)
    return (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", header) + chunk(b"IDAT", zlib.compress(rows))
            + chunk(b"IEND", b""))


answers = []


def record(response):
    request = response.request
    if request.url.path.startswith("/api/public/media"):
        response.read()
        entry = {"method": request.method, "path": request.url.path, "status": response.status_code}
        if request.method == "POST" and response.status_code == 200:
            entry["body"] = response.json()
        answers.append(entry)


picture = noise_png(64)
url = "data:image/png;base64," + base64.b64encode(picture).decode()

client = Langfuse(
    public_key=os.environ["SMOKE_PUBLIC_KEY"],
    secret_key=os.environ["SMOKE_SECRET_KEY"],
    host=host,
    httpx_client=httpx.Client(event_hooks={"response": [record]}),
)

traces = []
for n in range(2):
    with client.start_as_current_observation(
        name=f"smoke-media-{n}",
        as_type="generation",
        model="claude-sonnet-5",
        input=[{"role": "user", "content": [
            {"type": "text", "text": "what is in this picture"},
            {"type": "image_url", "image_url": {"url": url}},
        ]}],
    ) as generation:
        traces.append(generation.trace_id)
        time.sleep(1.5)
        generation.update(output={"role": "assistant", "content": "noise"})

client.flush()
client.shutdown()

with open(out_file, "w") as f:
    json.dump({"traces": traces, "answers": answers, "size": len(picture),
               "picture": base64.b64encode(picture).decode()}, f)
print(f"langfuse media traces {traces} exported to {host}")
