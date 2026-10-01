# Lumi

Personal, model-agnostic assistant harness in Go. One local binary: agent loop, modules, memory — no server.
Design: [docs/design.md](docs/design.md) · Requirements: [docs/requirement.txt](docs/requirement.txt)

## Requirements

- Go 1.26+ (no C toolchain needed; `CGO_ENABLED=0`)
- An API key for any OpenAI-compatible provider
- Optional: [go-task](https://taskfile.dev), `golangci-lint`

## Build & test

```powershell
$env:CGO_ENABLED="0"
go build -trimpath -o bin/ ./cmd/lumi   # -> bin/lumi.exe
go test ./...
go vet ./...
```

With go-task: `task build` · `task test` · `task lint` · `task chat` (loads `.env`).

## Setup

1. Write a config template to `~/.lumi/config.yaml` (override the location with `LUMI_HOME`):
   ```powershell
   .\bin\lumi.exe config init
   ```
2. Declare a provider and map the three tiers to models:
   ```yaml
   providers:
     my-provider:
       kind: openai-compatible          # wire protocol adapter
       base_url: https://api.example.com/v1
       api_key_env: MY_PROVIDER_API_KEY # optional
   tiers:
     fast:      { provider: my-provider, model: small-model }
     reasoning: { provider: my-provider, model: large-model, params: { reasoning_effort: high } }
     cheap:     { provider: my-provider, model: small-model }
   ```
   `params` are sent to the provider as-is. Set `no_temperature: true` on a tier if its model rejects `temperature`.
3. Provide the API key. Lumi checks these in order:
   `LUMI_<PROVIDER>_API_KEY` env → `api_key_env` env → OS keyring (`lumi key set <provider>`, recommended).

## Usage

```powershell
lumi chat                          # interactive, default module
lumi chat -m english-coach         # pick a module
lumi chat "what is a B-tree?"      # one-shot
lumi chat -s <session-id>          # resume a session
lumi module list | show <name> | validate <file>
lumi eval [-v] [suite.yaml...]     # score modules against golden conversations (default evals/*.yaml)
lumi key set <provider> | delete <provider>
lumi memory list | forget <id|key> | clear
lumi config init [--force] | path
```

### Voice on Linux

Voice mode shells out to the system's audio tools, trying them in order and
verifying each one actually delivers samples:

| Role | Tried in order |
|------|----------------|
| capture | `pw-record`, `pw-cat`, `parecord`, `ffmpeg`, `arecord` |
| playback | `paplay`, `pw-play`, `aplay`, `play`, `ffplay`, `cvlc`, `mpv`, `gst-play-1.0` |

Install whichever your distro ships — `pipewire-utils` (PipeWire), `alsa-utils`
(plain ALSA), or `ffmpeg` — and no configuration is needed. Override with
`LUMI_AUDIO_RECORD` / `LUMI_AUDIO_PLAY` (plus `LUMI_AUDIO_RECORD_ARGS` /
`LUMI_AUDIO_PLAY_ARGS` for extra flags). A tool that starts but stays silent is
skipped automatically rather than yielding an empty recording.

Recording is requested at 16 kHz mono, which the tools resample from the device
as needed.

In chat: `Ctrl+C` interrupts the reply, `Ctrl+D` or `/exit` quits. Output is colored and wrapped on a terminal, plain when piped or with `NO_COLOR=1`. Add `--debug` for logs.

## Modules

Built-in: `daily-assistant`, `english-coach`, `interview-coach` ([modules/](modules)).
A module is YAML (persona, system prompt, tier). Drop a file with the same name into `~/.lumi/modules/` to override a built-in, or a new name to add one.
With `feedback: true`, the model appends a `<feedback>` block; Lumi strips it from the reply and shows corrections separately (never spoken in voice mode).

## Evals

Prompt changes are measured, not guessed. `evals/*.yaml` replays scripted conversations in a throwaway database and applies deterministic checks (question ratio, banned/repeated phrases, expected fixes, no false fixes, no feedback leaks). Run `lumi eval -v` after every prompt edit.

## Configuration precedence

built-in defaults → `~/.lumi/config.yaml` → `LUMI_*` env vars (`__` = nesting, e.g. `LUMI_TIERS__FAST__MODEL=x`).

## Memory

Lumi learns durable facts and carries them between sessions, so it stops asking
what it was already told. At the end of a session one cheap model call distils
the conversation into a handful of short facts (`profile_facts`). The next
session injects them as a capped block in its system prompt.

Only distilled facts are injected — never raw transcripts — and `max_chars` caps
the block, so **per-turn token cost stays flat** (~150 tokens) whether you have
10 facts or 5,000. Facts are keyed (`role`, `city`, …), so a later correction
replaces the old fact instead of contradicting it.

```yaml
memory:
  enabled: true
  max_facts: 8            # most facts one reflection may add or update
  max_chars: 600          # hard cap on the injected block
  min_turns: 4            # sessions shorter than this are not reflected on
  max_reflect_chars: 6000 # transcript budget for the reflection call
```

Inspect and edit what it knows:

```
lumi memory list              # every stored fact, with confidence and use count
lumi memory forget <id|key>   # delete one
lumi memory clear             # delete them all
```

## Data

All data stays local in `~/.lumi/`: `lumi.db` (sessions, turns), `history` (prompt history), `config.yaml`, `modules/`. Delete the directory to erase everything.

## Layout

```
cmd/lumi/                 CLI (cobra)
internal/engine/          turn loop, event stream (text | feedback | done)
internal/coach/           feedback splitter/parser
internal/ui/              terminal rendering (lipgloss)
internal/eval/            golden-conversation scoring
internal/memory/           session reflection, fact store, capped injection
internal/provider/llm/    LLM interface, tiers; openaicompat/ adapter
internal/config|module|store|secrets|app
modules/                  built-in module YAML (embedded)
evals/                    golden conversations
docs/                     design and requirements
```
