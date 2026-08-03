// Package web serves the site, compiled into the binary.
//
// Embedding rather than mounting a volume means one artifact to deploy and no
// chance of the binary and the HTML drifting apart. The whole site is a few KB;
// if it grows past a megabyte or two, revisit.
//
// Every page is rendered from the résumé data once at construction, into
// []byte, and those bytes are served verbatim per request. A template that
// fails to parse or execute is returned as an error from New so main can make
// it fatal — a broken template stops the binary from booting rather than
// surfacing as a blank page in production.
package web

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"github.com/AnurupBansal/portfolio/internal/resume"
)

//go:embed templates
var templatesFS embed.FS

// htmlCache matches the previous static handler's policy for HTML: revalidate
// every time, since the markup changes every deploy.
const htmlCache = "public, max-age=0, must-revalidate"

// Server holds the pre-rendered pages. Constructed once via New.
type Server struct {
	index      []byte
	resume     []byte
	playground []byte
}

// pageData is what each template is executed with. Embedding *resume.Resume
// promotes its fields and methods (.Name, .Systems, .Featured, .Tags, …) to the
// top level, while Title/Description carry the per-page <head> values and Nav
// tells the shared header which link to mark aria-current.
type pageData struct {
	Title       string
	Description string
	Nav         string
	*resume.Resume
}

// New parses the templates and renders every page into bytes. Any parse or
// execute error is returned; callers should treat it as fatal.
func New(r *resume.Resume) (*Server, error) {
	// icons resolves a stack tag to its sprite symbol id, drawn from the same
	// source the résumé's stack groups use so an icon can't disagree with
	// itself across the page.
	icons := r.Icons()

	funcs := template.FuncMap{
		"icon": func(tag string) string {
			if id := icons[tag]; id != "" {
				return id
			}
			return "i-box"
		},
		"inc":  func(i int) int { return i + 1 },
		"mul":  func(a, b int) int { return a * b },
		"join": strings.Join,
	}

	tmpl, err := template.New("site").Funcs(funcs).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return nil, err
	}

	s := &Server{}
	pages := []struct {
		name string
		dst  *[]byte
		data pageData
	}{
		{"index.html", &s.index, pageData{
			Title:       r.Name + " — " + r.Role,
			Description: r.Tagline,
			Nav:         "home",
			Resume:      r,
		}},
		{"resume.html", &s.resume, pageData{
			Title:       r.Name + " — Résumé",
			Description: r.Tagline,
			Nav:         "resume",
			Resume:      r,
		}},
		{"playground.html", &s.playground, pageData{
			Title:       r.Name + " — Playground",
			Description: "The site as a system you can hit: a live request trace, build states, and the endpoints behind this page.",
			Nav:         "playground",
			Resume:      r,
		}},
	}
	for _, p := range pages {
		body, err := render(tmpl, p.name, p.data)
		if err != nil {
			return nil, fmt.Errorf("rendering %s: %w", p.name, err)
		}
		*p.dst = body
	}
	return s, nil
}

func render(t *template.Template, name string, data pageData) ([]byte, error) {
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Index serves the home page. Register it on "GET /{$}" so only the exact root
// path matches — every other unknown path falls through to the mux's 404.
func (s *Server) Index() http.HandlerFunc { return s.page(s.index) }

// Resume serves the résumé page.
func (s *Server) Resume() http.HandlerFunc { return s.page(s.resume) }

// Playground serves the playground page (live trace, build states, endpoints).
func (s *Server) Playground() http.HandlerFunc { return s.page(s.playground) }

func (s *Server) page(body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", htmlCache)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}
}
