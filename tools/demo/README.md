# Demo GIF

`run.sh` records `docs/demo.gif`, the short tour of the settings UI shown at the top of the README.

It builds give-me-real-gpu into a temporary directory, starts it on loopback ports with an empty data directory,
saves settings and three sinks through the settings API (dry run, so nothing is written anywhere; all addresses
are documentation addresses), drives the UI in headless Chrome with `record.mjs`, records it with a screencast and
turns the frames into a GIF with ffmpeg. Everything it saves goes to the temporary data directory. Everything it
started is stopped and the temporary directory is removed on exit.

## Scenes and captions

A caption bar at the bottom of the page says what each scene shows. It is drawn in the top layer, so it stays
above open dialogs, and it is in the page language.

| Scene | What happens | Caption (`en`) | Caption (`ko`) |
|-------|--------------|----------------|----------------|
| `status` | the cursor goes over the OpenStack values and the sink table | Status: what to register in the management platform, and lines per sink | 상태: 관리 플랫폼에 등록할 값과 sink 별 줄 수 |
| `general` | the cursor goes over the fields, the write interval becomes `15s`, Save shows the saved notice | General: change a setting and save. It applies without a restart | 일반: 값을 바꿔 저장하면 재시작 없이 반영됩니다 |
| `sinks` | Add sink, the InfluxDB version goes v1, v2, v3, the type goes to OTLP, Cancel | Add sink: the fields follow the type, InfluxDB v1, v2, v3 or OTLP | Sink 추가: 종류와 버전(InfluxDB v1, v2, v3, OTLP)에 맞춰 칸이 바뀝니다 |
| | Edit on site-b shows its three tag rules, Cancel | Tag rules: rename, drop or add tag keys per sink before writing | 태그 규칙: sink 마다 태그 키를 바꾸거나 빼거나 더해서 보냅니다 |
| `topology` | a VM from the palette goes onto a GPU with room, the same VM dragged onto a full GPU is refused | Topology: drag a VM onto a GPU. A GPU with no room refuses it | 토폴로지: VM 을 GPU 에 끌어다 놓습니다. 자리가 없는 GPU 는 거부합니다 |
| | Save shows the saved toast | Save: the fake OpenStack API serves the new topology right away | 저장: 가짜 OpenStack API 가 새 토폴로지를 바로 내보냅니다 |

The captions are in `CAPTIONS` at the top of `record.mjs`.

## Requirements

- Go, to build the binary
- Node.js 20 or later (no npm packages; on Node 20 the script adds `--experimental-websocket` by itself)
- Google Chrome or Chromium
- ffmpeg and ffprobe
- curl

## Run

```sh
tools/demo/run.sh                  # writes docs/demo.gif
tools/demo/run.sh /path/to/out.gif
DEMO_LANG=ko tools/demo/run.sh     # writes docs/demo.ko.gif
DEMO_SCENES=topology tools/demo/run.sh /tmp/topology.gif   # one scene, for trying changes
```

The English and the Korean GIF can be recorded at the same time when each run has its own ports, for example
`DEMO_LISTEN_PORT=27710 DEMO_UI_PORT=27790` for the second one. Each run makes its own temporary directory and
Chrome profile and lets Chrome pick a free DevTools port.

The page is recorded at 1280x800 with the dark theme. Chrome sends a screencast frame whenever the page changes and
at most `DEMO_FPS` frames a second are kept. The GIF is scaled to 960 px wide; when it is larger than the size
limit, the frame rate and the number of colors are lowered step by step, and when that is not enough the GIF is
made narrower (7/8, then 3/4 of the width).

## Environment variables

| Variable | Default | Meaning |
|----------|---------|---------|
| `DEMO_LANG` | `en` | page language, `en` or `ko`. `ko` writes `docs/demo.ko.gif` by default |
| `DEMO_LISTEN_PORT` | `27700` | loopback port of the fake OpenStack API |
| `DEMO_UI_PORT` | `27780` | loopback port of the settings UI |
| `DEMO_FPS` | `10` | most frames kept per second and the first GIF frame rate tried |
| `DEMO_WIDTH` | `960` | GIF width in pixels |
| `DEMO_MAX_BYTES` | `6291456` | size limit of the GIF (6 MiB) |
| `DEMO_SCENES` | all | scenes to record, comma separated: `status`, `general`, `sinks`, `topology` |
| `DEMO_VERSION` | `git describe --tags --always` | version shown in the page footer |
| `DEMO_KEEP_WORK` | `0` | `1` keeps the temporary directory (frames, logs) |
| `DEMO_WORK_PARENT` | `$TMPDIR` or `/tmp` | where the temporary directory is made |
| `DEMO_THEME_KEY` | `gmrg.theme` | localStorage key of the theme choice |
| `DEMO_LANG_KEY` | `gmrg.lang` | localStorage key of the language choice |
| `CHROME` | `google-chrome` | Chrome executable |
| `NODE` | `node` | Node.js executable |

The ports must be free; the script stops if either is in use, and it never uses 15000 or 15080, the default
ports of an emulator that may be running on the same host.
