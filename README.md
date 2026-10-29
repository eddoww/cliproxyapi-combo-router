# cliproxyapi-combo-router

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) v8 plugin that adds **model combos**
and **capability-aware routing** to the proxy.

Everything upstream keeps working untouched: this is a standard dynamic-library plugin speaking the
proxy's C ABI, so it does not change the CPA binary and does not depend on the Go version CPA was
compiled with.

## What it does

**Combos.** A combo is a client-visible model name that expands to an ordered list of real models.
Requests to a combo are tried member by member until one succeeds.

```jsonc
// request
{"model": "smart", "messages": [...]}
// tried: grok-4.7 -> deepseek-v4-pro -> gpt-oss-120b
```

**Capability-aware ordering.** On every request the plugin inspects the *current user turn* for
images, audio, video, PDFs and tools. Members that advertise the needed capabilities are floated to
the front. Members are **never dropped** by capability ordering, so fallback stays intact. Empty entries,
duplicates and a combo naming itself are removed at load time.

```
combo "smart": [deepseek-v4-pro (no vision), grok-4.7 (vision)]
  text request   -> deepseek-v4-pro
  image request  -> grok-4.7   (vision floats ahead)
```

Only the trailing user turn is inspected, so an image earlier in a conversation does not pin the
whole session to a vision model.

**Cross-model fallback.** A member hands over to the next one on `401, 403, 404, 408, 429`, any
`5xx`, and transport errors. Genuine request errors (`400, 413, 422`) are terminal and are returned
with their upstream HTTP status. A member that returns `2xx` with an empty body is treated as a
failed attempt so the next member can answer. Streaming falls back the same way, but only while
**no meaningful payload has reached the client**: once output is emitted the stream is committed to
that member, because switching would splice two answers together.

**Round-robin.** Optional `strategy: round-robin` per combo spreads requests across members instead
of always preferring the first one. Rotation advances exactly once per execution. Capability
ordering still applies within the rotation. `default_strategy` applies to combos that omit their own
`strategy`.

## Install

1. Drop `combo-router.so` into the proxy's plugin directory (default `<writable>/plugins`).
2. Enable it in `config.yaml`:

```yaml
plugins:
  enabled: true
  dir: "/data/plugins"        # must be the same directory as above
  configs:
    combo-router:
      enabled: true
      priority: 10
      default_strategy: fallback
      combos:
        - name: smart
          models:
            - grok-4.7
            - deepseek-v4-pro
            - gpt-oss-120b
        - name: vision
          models:
            - grok-4.7
            - gemini-2.5-flash
          strategy: fallback
      capabilities:            # optional per-model overrides
        my-private-model:
          - vision
          - tools
```

The plugin id (`combo-router`) is derived from the file name, so the config key and the file name
must match.

Restart the proxy after adding or replacing the shared library.

## Configuration reference

| Key | Default | Meaning |
| --- | --- | --- |
| `enabled` | `true` | Master switch. When false every combo request falls through to normal routing. |
| `default_strategy` | `fallback` | `fallback` or `round-robin` for combos without their own strategy. |
| `combos[].name` | — | Client-visible model id. |
| `combos[].models` | — | Ordered members. Prefixed ids (`openrouter/gpt-5`) are passed through as-is. |
| `combos[].strategy` | inherits | Overrides `default_strategy`. |
| `capabilities` | — | Per-model capability override: `vision`, `audio`, `video`, `pdf`, `tools`. Replaces built-ins. |

Built-in capability prefixes live in `catalog.go`: Claude, GPT-4/5, o-series and Gemini get vision
and PDF (Gemini also audio/video); Grok 3/4 get vision; `gpt-oss`, `gpt-3`, `grok-2` and
DeepSeek/Mistral/Llama/Qwen advertise tools only. Unknown models advertise nothing, which sorts them
behind known-capable members but keeps them in the chain. Override keys may be qualified
(`openrouter/gpt-5`) or bare (`gpt-5`); both are honoured.

## Build

The plugin is a `c-shared` library, built from this repository:

```bash
go test ./...
go build -buildvcs=false -buildmode=c-shared -o dist/combo-router.so .
```

CI builds the same artifact inside `golang:1.26.10-bookworm` and attaches it to tagged releases.

## Deploying on Kubernetes

The plugin binary is not part of the upstream image, so mount it into the data volume from an init
container:

```yaml
initContainers:
  - name: combo-router-plugin
    image: busybox:1.36
    command:
      - /bin/sh
      - -ec
      - mkdir -p /data/plugins &&
        wget -qO /data/plugins/combo-router.so
        https://github.com/eddoww/cliproxyapi-combo-router/releases/download/v0.1.0/combo-router.so
    volumeMounts:
      - {name: data, mountPath: /data}
```

## Limits

- The plugin cannot add new HTTP routes. `/v1/embeddings`, `/v1/audio/transcriptions` and a
  TypeSafe/`/v1/systemone` endpoint need proxy-level routes and therefore an edge service or a
  small fork.
- Combos are resolved by model name only; they are not listed in `/v1/models` unless a provider
  credential advertises them (clients can still request them by name).
- Media in *earlier* conversation turns is ignored when choosing a member, even though the request
  still carries it. A text follow-up can therefore select a member that would reject the historical
  image; configure the combo so the first member can handle your history, or strip history upstream.
- Token counting returns a zero estimate; the combo executor does not own a tokenizer.
