# Synergia API — OpenAI-compatible contract

Synergia exposes an OpenAI-compatible HTTP API. Any client that works with the OpenAI SDK can target Synergia by changing the `base_url` and `api_key`.

---

## Authentication

Every request must carry the cluster API key in one of two ways:

```
Authorization: Bearer <api_key>
X-API-Key: <api_key>
```

Requests without a valid key receive `401 Unauthorized`.

---

## The `model` field

**Always set the `model` field.** Use the **model name** (e.g. `SmolLM2-135M-Instruct`, `bge-m3`) — not the filename (e.g. `SmolLM2-135M-Instruct-Q4_K_M.gguf`). The model name is what the worker registers with at connect time.

Currently the field is **pass-through**: the manager dispatches to the single available worker regardless of the value, and the response echoes it back. Routing by model/role will be enforced once multi-worker deployments and role-prefixed endpoints are introduced (see below).

Until then, set it to the model name the worker is running — this keeps responses OpenAI-compatible and prepares callers for when routing is enforced.

---

## Endpoints

### `POST /v1/chat/completions` — synchronous inference

Blocks until a worker returns a result or the request times out (default 120 s).

**Request**
```json
{
  "model": "inference",
  "messages": [
    { "role": "system", "content": "You are a helpful assistant." },
    { "role": "user",   "content": "Summarise this document in three bullet points." }
  ],
  "temperature": 0.7,
  "max_tokens": 512,
  "response_format": { "type": "json_object" }
}
```

`temperature`, `max_tokens`, and `response_format` are optional.

**Response** — OpenAI `chat.completion` object
```json
{
  "id": "chatcmpl-<uuid>",
  "object": "chat.completion",
  "model": "inference",
  "choices": [
    {
      "index": 0,
      "message": { "role": "assistant", "content": "..." },
      "finish_reason": "stop"
    }
  ],
  "usage": { "prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0 }
}
```

**Error responses**

| Code | Meaning |
|------|---------|
| `401` | Missing or invalid API key |
| `429` | No worker available — all workers are busy, paused, updating, or withdrawn |
| `503` | No worker connected |
| `408` | Request timed out waiting for a worker result |

---

### `POST /v1/batches` — async queue

Enqueues a request and returns immediately with a batch object. The manager dispatches it when a worker becomes available. Use this for non-interactive workloads where latency does not matter.

**Request** — same body as `/v1/chat/completions`
```json
{
  "model": "ingestion",
  "messages": [
    { "role": "user", "content": "Extract all named entities from this text: ..." }
  ]
}
```

**Response** `202 Accepted`
```json
{
  "id": "batch_<uuid>",
  "object": "batch",
  "status": "pending",
  "model": "ingestion",
  "endpoint": "/v1/chat/completions",
  "created_at": 1747039200,
  "request_counts": { "total": 1, "completed": 0, "failed": 0 }
}
```

---

### `GET /v1/batches/{id}` — poll batch status

```
GET /v1/batches/batch_abc123
```

**Response** — same batch object, with `status` updated:

| Status | Meaning |
|--------|---------|
| `pending` | Queued, not yet dispatched |
| `in_progress` | Dispatched to a worker |
| `completed` | Finished — result is in `output` |
| `failed` | Worker returned an error — reason is in `error` |
| `cancelled` | Cancelled by the caller |
| `expired` | Timed out in queue |

When `status` is `completed`, the batch object includes:
```json
{
  "status": "completed",
  "output": { "choices": [{ "message": { "role": "assistant", "content": "..." } }] },
  "completed_at": 1747039260
}
```

---

### `GET /v1/batches` — list recent batches

Returns the 100 most recent batch requests (newest first).

---

### `POST /v1/batches/{id}/cancel`

Cancels a `pending` batch. Returns `409 Conflict` if the batch has already started or finished.

---

### `GET /v1/models` — list active roles

Returns the roles currently configured in the cluster with their associated model names.

```json
[
  { "id": "tester",    "object": "model", "owned_by": "synergia" },
  { "id": "embedding", "object": "model", "owned_by": "synergia" },
  { "id": "inference", "object": "model", "owned_by": "synergia" },
  { "id": "ingestion", "object": "model", "owned_by": "synergia" }
]
```

---

## OpenAI SDK usage

```python
from openai import OpenAI

client = OpenAI(
    base_url="https://synergia.example.com/v1",
    api_key="your-api-key",
)

# Synchronous inference
response = client.chat.completions.create(
    model="inference",
    messages=[{"role": "user", "content": "What is 2 + 2?"}],
)
print(response.choices[0].message.content)
```

For embeddings, point the client at the embedding endpoint directly once role-prefixed endpoints are available (see below).

---

## Planned: role-prefixed endpoints

A future release will add per-role base paths so that callers can set a single `base_url` per use case without repeating the `model` field:

| Base URL | Role |
|----------|------|
| `.../v1/tester`    | `POST /v1/tester/chat/completions` |
| `.../v1/embedding` | `POST /v1/embedding/embeddings` |
| `.../v1/inference` | `POST /v1/inference/chat/completions` |
| `.../v1/ingestion` | `POST /v1/ingestion/chat/completions` |

Until then, use the flat endpoints above and always include `"model": "<role>"`.
