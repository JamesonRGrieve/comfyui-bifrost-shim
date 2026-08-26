# comfyui-bifrost-shim

An OpenAI-compatible **image-generation** shim in front of [ComfyUI](https://github.com/comfyanonymous/ComfyUI),
so ComfyUI can be registered as a custom OpenAI provider in the
[bifrost](https://github.com/maximhq/bifrost) gateway (or used by any OpenAI-images client).

ComfyUI's native API is a workflow graph (`POST /prompt`). This shim exposes
`GET /v1/models` and `POST /v1/images/generations`, mapping each request to a ComfyUI
workflow template and returning the rendered image in the OpenAI response shape.

Single static Go binary, standard library only — no runtime dependencies.

## How it works

Each entry in `models.json` is one OpenAI *model* bound to a workflow template (ComfyUI
API/prompt format) under `workflows/`. Per-model `bindings` name which node input carries
the prompt / negative / seed / size / checkpoint, so one generic submit path drives every
workflow. Add a model by dropping in a workflow JSON and a `models.json` entry — no code
change, and (when deployed via IaC) no new binary release.

```
POST /v1/images/generations
  { "model": "sdxl-realvis", "prompt": "...", "size": "1024x1024", "n": 1 }
      │
      ▼  build workflow from template + bindings
  ComfyUI  POST /prompt → poll /history/{id} → GET /view
      │
      ▼  { "created": ..., "data": [ { "b64_json": "<png>" } ] }
```

## Configuration

| Env | Default | Meaning |
|-----|---------|---------|
| `COMFYUI_URL` | `http://127.0.0.1:8188` | Upstream ComfyUI base URL |
| `SHIM_ADDR` | `0.0.0.0:8189` | Listen address |
| `SHIM_CONFIG` | `./config/models.json` | Path to the model map (`workflows/` is resolved beside it) |

`models.json`:

```json
{
  "default_model": "sdxl-realvis",
  "models": {
    "sdxl-realvis": {
      "workflow": "sdxl.json",
      "checkpoint": "RealVisXL_V5.0_fp16.safetensors",
      "bindings": {
        "positive": ["6", "text"], "negative": ["7", "text"],
        "checkpoint": ["4", "ckpt_name"], "seed": ["3", "seed"],
        "width": ["5", "width"], "height": ["5", "height"],
        "steps": ["3", "steps"], "cfg": ["3", "cfg"]
      },
      "defaults": { "steps": 30, "cfg": 7.0, "negative": "low quality, blurry" }
    }
  }
}
```

A binding is `[node_id, input_field]` into the workflow graph. `defaults` supplies values for
bound `steps`/`cfg`/`negative`/etc.

## Build & run

```bash
go build -o comfyui-bifrost-shim .
COMFYUI_URL=http://127.0.0.1:8188 ./comfyui-bifrost-shim
```

## Deploy

Tagging `vX.Y.Z` publishes a static `linux/amd64` binary as a GitHub release asset
(`.github/workflows/release.yml`). Infrastructure pulls that asset (e.g. OpenTofu `host_binary`)
and runs it as a systemd unit beside ComfyUI; the `models.json` + `workflows/` are deployed as
config so they can be updated without a new binary.

Register in bifrost as a custom OpenAI provider pointing at `http://<shim-host>:8189/v1`;
bifrost then forwards `/v1/images/generations` to it.

## License

AGPL-3.0-or-later.
