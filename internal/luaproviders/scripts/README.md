# Bundled Lua providers

Team-maintained provider scripts embedded into the binary via
`//go:embed`. Layout — one directory per provider, the directory name
IS the provider id (the script's `id` field must match it):

    scripts/<id>/main.lua

A bundled script shadows nothing and shadows nothing's absence: the
factory assembles `scripts/` FIRST in the LoadSources precedence
(bundled → [providers.lua].dir → ~/.config/anicli/providers), so a
user directory can override a bundled script by id without a rebuild
— and a bundled script shadows the compiled Go factory with the same
id (Lua shadows Go, PR116).

Scripts program against the `anicli` SDK (internal/lua/sdk.go): http
(get/get_json/get_batch/post/query_escape), json, html, regexp,
base64, time, log, extract — the shared Go extractor factory — plus
the sandbox budgets (timeouts, body caps, no io/os/debug).
