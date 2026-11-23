package archiv

import (
	"bytes"
	"html/template"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

// Page texts of tags are Markdown. Raw HTML in them is not passed through,
// and links with dangerous schemes are dropped, so a page text cannot run
// script on the site.
var md = goldmark.New(goldmark.WithExtensions(extension.GFM))

func renderMarkdown(s string) template.HTML {
	var b bytes.Buffer
	if err := md.Convert([]byte(s), &b); err != nil {
		return template.HTML(template.HTMLEscapeString(s))
	}
	return template.HTML(b.String())
}
