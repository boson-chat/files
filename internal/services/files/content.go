package files

import (
	"mime"
	"net/http"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

// inlineTypes are the only types served inline (rendered by the browser).
// Everything else, svg and html included, is served as an attachment.
var inlineTypes = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true, "image/avif": true,
	"video/mp4": true, "video/webm": true,
	"audio/mpeg": true, "audio/mp3": true, "audio/ogg": true, "video/ogg": true, "application/ogg": true,
	"audio/wav": true, "audio/wave": true, "audio/x-wav": true, "audio/vnd.wave": true,
	"application/pdf": true,
	"text/plain":      true,
}

// imageTypes get their dimensions recorded.
var imageTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// ServePolicy is how a stored type is served.
type ServePolicy struct {
	// ContentType is the response Content-Type.
	ContentType string
	Inline      bool
}

// PolicyFor decides the response type and disposition for a stored type.
// Inline types keep their type (text/plain as text/plain; charset=utf-8);
// anything else is an attachment of type application/octet-stream, so no
// browser renders it.
func PolicyFor(stored string) ServePolicy {
	mt := baseType(stored)
	if !inlineTypes[mt] {
		return ServePolicy{ContentType: "application/octet-stream", Inline: false}
	}
	if mt == "text/plain" {
		return ServePolicy{ContentType: "text/plain; charset=utf-8", Inline: true}
	}
	return ServePolicy{ContentType: mt, Inline: true}
}

// ContentDisposition renders the header for name (RFC 6266; non-ASCII
// names get filename*).
func ContentDisposition(inline bool, name string) string {
	kind := "attachment"
	if inline {
		kind = "inline"
	}
	if v := mime.FormatMediaType(kind, map[string]string{"filename": name}); v != "" {
		return v
	}
	return kind
}

func baseType(ct string) string {
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return ""
	}
	return strings.ToLower(mt)
}

// DetectType picks the stored type from the declared Content-Type and the
// first bytes of the body. A missing/generic declaration takes the sniffed
// type; a body that sniffs as HTML or XML is never stored under an inline
// media type (it would be served inline otherwise).
func DetectType(declared string, head []byte) string {
	mt := baseType(declared)
	if len(mt) > 127 {
		mt = ""
	}
	sniffed := baseType(http.DetectContentType(head))
	if mt == "" || mt == "application/octet-stream" {
		if sniffed == "" {
			return "application/octet-stream"
		}
		return sniffed
	}
	if inlineTypes[mt] && mt != "text/plain" && (sniffed == "text/html" || strings.HasSuffix(sniffed, "xml")) {
		return "application/octet-stream"
	}
	if mt == "text/plain" && sniffed == "text/html" {
		return "text/html"
	}
	if mt == "text/plain" {
		return "text/plain"
	}
	if params := paramsOf(declared); params != "" && strings.HasPrefix(mt, "text/") {
		return mt + "; " + params
	}
	return mt
}

func paramsOf(ct string) string {
	_, params, err := mime.ParseMediaType(ct)
	if err != nil {
		return ""
	}
	if cs, ok := params["charset"]; ok {
		return "charset=" + cs
	}
	return ""
}

// MaxNameBytes caps stored names.
const MaxNameBytes = 200

// SanitizeName makes a safe display/download name: the last path element,
// no control or bidi-override characters, trimmed, at most MaxNameBytes
// (keeping the extension), "file" when nothing is left.
func SanitizeName(name string) string {
	if !utf8.ValidString(name) {
		name = strings.ToValidUTF8(name, "")
	}
	name = strings.ReplaceAll(name, "\\", "/")
	if name = path.Base("/" + name); name == "/" {
		name = ""
	}
	name = strings.Map(func(r rune) rune {
		switch {
		case unicode.IsControl(r), r == '\u202a', r == '\u202b', r == '\u202c', r == '\u202d', r == '\u202e',
			r == '\u2066', r == '\u2067', r == '\u2068', r == '\u2069', r == '\u200e', r == '\u200f', r == '\ufeff':
			return -1
		case r == '"' || r == '/' || r == ':' || r == '*' || r == '?' || r == '<' || r == '>' || r == '|':
			return '_'
		}
		return r
	}, name)
	name = strings.Trim(strings.TrimSpace(name), ".")
	if name == "" {
		return "file"
	}
	if len(name) > MaxNameBytes {
		ext := path.Ext(name)
		if len(ext) > 16 {
			ext = ""
		}
		stem := name[:len(name)-len(ext)]
		cut := MaxNameBytes - len(ext)
		for cut > 0 && !utf8.RuneStart(stem[cut]) {
			cut--
		}
		name = stem[:cut] + ext
	}
	return name
}
