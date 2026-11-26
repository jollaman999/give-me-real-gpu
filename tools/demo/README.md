# Demo GIF

`run.sh` records `docs/demo.gif`, the short tour of the settings UI shown at the top of the README.

It builds give-me-real-gpu into a temporary directory, starts it on loopback ports with an empty data directory,
saves settings and three sinks through the settings API (dry run, so nothing is written anywhere; all addresses
are documentation addresses), drives the UI in headless Chrome with `record.mjs` and turns the screenshots into
a GIF with ffmpeg. The tour: Status, General, the sink list, the site-b sink dialog with its tag rules, and the
Topology tab where a VM is dragged from the palette onto a GPU. Nothing is saved in the topology.
Everything it started is stopped and the temporary directory is removed on exit.

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
```

The page is recorded at 1280x800 in English with the dark theme. The GIF is scaled to 960 px wide; when it is
larger than the size limit, the frame rate and the number of colors are lowered step by step.

## Environment variables

| Variable | Default | Meaning |
|----------|---------|---------|
| `DEMO_LANG` | `en` | page language, `en` or `ko`. `ko` writes `docs/demo.ko.gif` by default |
| `DEMO_LISTEN_PORT` | `27700` | loopback port of the fake OpenStack API |
| `DEMO_UI_PORT` | `27780` | loopback port of the settings UI |
| `DEMO_FPS` | `10` | screenshots per second and the first GIF frame rate tried |
| `DEMO_WIDTH` | `960` | GIF width in pixels |
| `DEMO_MAX_BYTES` | `3145728` | size limit of the GIF |
| `DEMO_VERSION` | `git describe --tags --always` | version shown in the page footer |
| `DEMO_KEEP_WORK` | `0` | `1` keeps the temporary directory (frames, logs) |
| `DEMO_WORK_PARENT` | `$TMPDIR` or `/tmp` | where the temporary directory is made |
| `DEMO_THEME_KEY` | `gmrg.theme` | localStorage key of the theme choice |
| `DEMO_LANG_KEY` | `gmrg.lang` | localStorage key of the language choice |
| `CHROME` | `google-chrome` | Chrome executable |
| `NODE` | `node` | Node.js executable |

The ports must be free; the script stops if either is in use, and it never uses 15000 or 15080, the default
ports of an emulator that may be running on the same host.
