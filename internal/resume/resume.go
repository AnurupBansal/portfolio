// Package resume is the single source of truth for the résumé.
//
// One embedded JSON file feeds both the rendered page and /api/resume, so the
// two cannot drift. Load validates aggressively and is called at startup: a
// malformed résumé should stop the binary from booting, not surface as a blank
// section in production.
package resume

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

//go:embed data/resume.json
var raw []byte

type Link struct {
	Label string `json:"label"`
	Href  string `json:"href"`
	Text  string `json:"text"`
}

// StackItem is one technology, paired with the icon it renders with everywhere
// it appears (the stack filter, a system's chip list, the home "operates daily"
// row). Keeping the icon beside the name here is what stops those three places
// from drifting.
type StackItem struct {
	Name string `json:"name"`
	Icon string `json:"icon"`
}

type StackGroup struct {
	Label string      `json:"label"`
	Items []StackItem `json:"items"`
}

type Education struct {
	Degree string `json:"degree"`
	School string `json:"school"`
	Period string `json:"period"`
}

// Job is one role held, for the résumé's job-organised timeline. The résumé is
// primarily organised by System (what was built); Experience is the companion
// view for readers who scan by employer.
type Job struct {
	Title    string `json:"title"`
	Org      string `json:"org"`
	Location string `json:"location"`
	Period   string `json:"period"`
	Summary  string `json:"summary"`
	Current  bool   `json:"current"`
}

// System is one thing built, which is the unit this résumé is organised around
// rather than one job held.
type System struct {
	Slug     string   `json:"slug"`
	Title    string   `json:"title"`
	Org      string   `json:"org"`
	Period   string   `json:"period"`
	Headline string   `json:"headline"`
	Icon     string   `json:"icon"` // headline icon (a sprite symbol id, e.g. "i-net")
	Summary  string   `json:"summary"`
	HardPart string   `json:"hard_part"`
	Stack    []string `json:"stack"`
	Featured bool     `json:"featured"`
}

type Resume struct {
	Name       string       `json:"name"`
	Role       string       `json:"role"`
	Location   string       `json:"location"`
	Tagline    string       `json:"tagline"`
	Links      []Link       `json:"links"`
	Stack      []StackGroup `json:"stack"`
	Tools      []string     `json:"tools"` // things used daily but not built with (AI assistants)
	Experience []Job        `json:"experience"`
	Education  []Education  `json:"education"`
	Languages  []string     `json:"languages"`
	Systems    []System     `json:"systems"`
}

// Load parses and validates the embedded résumé.
func Load() (*Resume, error) {
	var r Resume
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields() // a typo'd key should fail loudly, not vanish
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("parsing resume.json: %w", err)
	}
	if err := r.validate(); err != nil {
		return nil, fmt.Errorf("invalid resume.json: %w", err)
	}
	return &r, nil
}

func (r *Resume) validate() error {
	if r.Name == "" || r.Role == "" {
		return fmt.Errorf("name and role are required")
	}
	if len(r.Systems) == 0 {
		return fmt.Errorf("no systems listed")
	}

	// Every tag a system claims must exist in a stack group. This keeps the
	// stack index provably complete: a tag can never be un-clickable, and a
	// typo ("Golang" vs "Go") fails the build instead of silently filtering to
	// nothing.
	known := map[string]bool{}
	for _, g := range r.Stack {
		for _, item := range g.Items {
			if item.Name == "" || item.Icon == "" {
				return fmt.Errorf("stack group %q has an item missing a name or icon", g.Label)
			}
			known[item.Name] = true
		}
	}

	seen := map[string]bool{}
	featured := 0
	for i, s := range r.Systems {
		switch {
		case s.Slug == "":
			return fmt.Errorf("systems[%d] (%q) has no slug", i, s.Title)
		case seen[s.Slug]:
			return fmt.Errorf("duplicate slug %q", s.Slug)
		case s.Title == "" || s.Summary == "":
			return fmt.Errorf("system %q needs a title and a summary", s.Slug)
		case s.Icon == "":
			return fmt.Errorf("system %q needs an icon", s.Slug)
		}
		seen[s.Slug] = true
		if s.Featured {
			featured++
		}
		for _, tag := range s.Stack {
			if !known[tag] {
				return fmt.Errorf("system %q claims %q, which is in no stack group", s.Slug, tag)
			}
		}
	}
	if featured == 0 {
		return fmt.Errorf("no featured systems; the main page index would be empty")
	}

	for i, j := range r.Experience {
		if j.Title == "" || j.Org == "" {
			return fmt.Errorf("experience[%d] needs a title and an org", i)
		}
	}
	return nil
}

// Featured returns the systems shown on the main page, in file order.
func (r *Resume) Featured() []System {
	out := make([]System, 0, 3)
	for _, s := range r.Systems {
		if s.Featured {
			out = append(out, s)
		}
	}
	return out
}

// Tags returns every stack tag actually used by a system, sorted. Tags listed
// in a stack group but used nowhere are excluded — the filter should only
// offer choices that lead somewhere.
func (r *Resume) Tags() []string {
	set := map[string]bool{}
	for _, s := range r.Systems {
		for _, tag := range s.Stack {
			set[tag] = true
		}
	}
	out := make([]string, 0, len(set))
	for tag := range set {
		out = append(out, tag)
	}
	sort.Strings(out)
	return out
}

// Icons maps every stack tag name to its sprite symbol id. The template uses it
// to draw a system's stack chips and the home "operates daily" row from the
// same source the filter buttons come from, so an icon can't disagree with
// itself across the page.
func (r *Resume) Icons() map[string]string {
	m := make(map[string]string)
	for _, g := range r.Stack {
		for _, item := range g.Items {
			m[item.Name] = item.Icon
		}
	}
	return m
}

// DraftCount reports how many systems still carry a placeholder (TODO)
// hard_part. main logs this at startup so the author knows what's outstanding;
// the template never renders these fields.
func (r *Resume) DraftCount() int {
	n := 0
	for _, s := range r.Systems {
		if strings.HasPrefix(s.HardPart, "TODO") {
			n++
		}
	}
	return n
}

// JSONHandler serves the résumé as JSON. A résumé you can curl is on-thesis for
// a site whose pitch is that visitors can hit real systems.
func (r *Resume) JSONHandler() http.HandlerFunc {
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		panic("marshalling resume: " + err.Error()) // unreachable: it round-trips
	}
	body = append(body, '\n')

	return func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=300")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}
}
