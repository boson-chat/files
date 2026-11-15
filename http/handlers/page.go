package handlers

import (
	"html/template"
	nethttp "net/http"
	"strconv"
)

var pageTmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>{{.Title}}</title>
<style>
:root { color-scheme: light dark; --bg: #f6f6f4; --fg: #1d1d1b; --muted: #6b6b66; --card: #ffffff; --line: #e3e3de; }
@media (prefers-color-scheme: dark) { :root { --bg: #131312; --fg: #ececea; --muted: #9a9a94; --card: #1c1c1a; --line: #2c2c29; } }
* { box-sizing: border-box; }
body { margin: 0; min-height: 100vh; display: grid; place-items: center; padding: 16px;
  background: var(--bg); color: var(--fg); font: 16px/1.5 system-ui, -apple-system, "Segoe UI", sans-serif; }
main { max-width: 26rem; width: 100%; background: var(--card); border: 1px solid var(--line); border-radius: 12px; padding: 28px 24px; }
.code { font: 600 13px/1 ui-monospace, SFMono-Regular, Menlo, monospace; color: var(--muted); letter-spacing: .04em; }
h1 { font-size: 1.25rem; margin: 10px 0 8px; }
p { margin: 0; color: var(--muted); }
</style>
</head>
<body>
<main>
<div class="code">{{.Code}}</div>
<h1>{{.Title}}</h1>
<p>{{.Text}}</p>
</main>
</body>
</html>
`))

// page writes a small self-contained HTML page (404 / 410 for files).
func page(w nethttp.ResponseWriter, status int, title, text string) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = pageTmpl.Execute(w, map[string]string{"Code": strconv.Itoa(status), "Title": title, "Text": text})
}
