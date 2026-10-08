# TeX SVG Engine

## What this worker does

This is the native replacement for the legacy TeX diagram renderer. It renders supported diagram sources to SVG with:

- `pdflatex` / `xelatex` (for CJK/non-ASCII fallback)
- `dvisvgm`
- no `--shell-escape`

Input path is the same JSON shape used by the Rin Renderer adapter:

```json
{
  "type": "tikzcd",
  "options": "",
  "body": "A \\\\arrow[r] & B",
  "source": ""
}
```

The response must contain:

- `id`
- `url`
- `svg`
- `cached`
- `type`

CloudBase upload and public URL signing remain in the Rin Renderer API layer.

## Supported diagram kinds

- `axis`, `pgfplots`
- `tikz`, `tikzpicture`
- `tikz-cd`
- `xymatrix`
- `pspicture`
- `picture`
- `forest`
- `circuitikz`
- `chemfig`, `chemfig-scheme`
- `amscd`

## Local run

```bash
go run ./engines/texsvg
```

With custom listen address:

```bash
RIN_RENDERER_ADDR=:8091 go run ./engines/texsvg
```

## Required environment

| Variable | Purpose |
|---|---|
| `RIN_RENDERER_SERVICE_TOKEN` | Optional service token checked by `X-Rin-Renderer-Token` / `Authorization: Bearer ...` |
| `RIN_RENDERER_ADDR` | HTTP listen address (default `:8091`) |
| `RIN_RENDERER_TEXSVG_MAX_BODY_BYTES` | Max request payload bytes (default `131072`) |
| `RIN_RENDERER_TEXSVG_RENDER_TIMEOUT_SECONDS` | Total request timeout (default `25s`) |
| `RIN_RENDERER_TEXSVG_COMMAND_TIMEOUT_SECONDS` | Per-command timeout (default `20s`) |
| `RIN_RENDERER_PDFLATEX_BIN` | `pdflatex` binary path (default `pdflatex`) |
| `RIN_RENDERER_XELATEX_BIN` | `xelatex` binary path (default `xelatex`) |
| `RIN_RENDERER_DVISVGM_BIN` | `dvisvgm` binary path (default `dvisvgm`) |
| `RIN_RENDERER_DVISVGM_ARGS` | Extra args passed to `dvisvgm` |
| `RIN_RENDERER_TEXSVG_ENGINE_ID` | Response `engine`/version metadata |
