package site

import (
	"bytes"
	_ "embed"
	"html/template"
)

// Labels holds every fixed UI string rendered in the HTML chrome and generated
// sections of the static site.
type Labels struct {
	Contents    string
	Previous    string
	Next        string
	ReadingPath string
	Reference   string
	Answers     string
	Code        string
	Report      string
	Evidence    string
	Rung        string
	Planned     string
}

// UILabels defines the default English labels used when rendering site pages.
var UILabels = Labels{
	Contents:    "Contents",
	Previous:    "Previous",
	Next:        "Next",
	ReadingPath: "Reading path",
	Reference:   "Reference",
	Answers:     "Answers",
	Code:        "Code",
	Report:      "Report",
	Evidence:    "Evidence",
	Rung:        "Rung",
	Planned:     "How this book was planned",
}

//go:embed style.css
var styleCSS []byte

//go:embed page.html
var pageTemplateText string

var pageTemplate = template.Must(template.New("page").Parse(pageTemplateText))

type navLink struct {
	URL   string
	Title string
}

type tocEntry struct {
	Level int
	Text  string
	ID    string
}

type experimentView struct {
	Title string
	URL   string
}

type chapterView struct {
	Title       string
	URL         string
	Rung        string
	Experiments []experimentView
}

type pageView struct {
	Labels      Labels
	Base        string
	BookTitle   string
	Title       string
	Home        bool
	Body        template.HTML
	TOC         []tocEntry
	Previous    *navLink
	Next        *navLink
	Answers     *navLink
	Chapter     *navLink
	ReadingPath []chapterView
	Reference   []navLink
	ReadingID   string
	ReferenceID string
}

func (b *builder) renderPage(p *page, body []byte) ([]byte, error) {
	view := pageView{
		Labels:    UILabels,
		Base:      b.opts.Base,
		BookTitle: b.home.title,
		Title:     p.title,
		Body:      template.HTML(body),
	}
	switch p.kind {
	case homePage:
		view.Home = true
		// Allocate generated section IDs from README's slugger so a README
		// heading named "Reading path" or "Reference" does not create duplicates.
		view.ReadingID = p.doc.ids.slug(UILabels.ReadingPath)
		view.ReferenceID = p.doc.ids.slug(UILabels.Reference)
		view.ReadingPath = b.readingPath()
		view.Reference = b.reference()
	case chapterPage:
		view.TOC = tableOfContents(p.doc)
		index := b.chapterIndex(p.chapter)
		if index > 0 {
			prev := b.chapters[index-1].page
			view.Previous = &navLink{URL: b.pageURL(prev), Title: prev.title}
		}
		if index+1 < len(b.chapters) {
			next := b.chapters[index+1].page
			view.Next = &navLink{URL: b.pageURL(next), Title: next.title}
		}
		if p.chapter.answers != nil {
			view.Answers = &navLink{URL: b.pageURL(p.chapter.answers), Title: UILabels.Answers}
		}
	case answersPage:
		view.Chapter = &navLink{URL: b.pageURL(p.chapter.page), Title: p.chapter.page.title}
	default:
		view.TOC = tableOfContents(p.doc)
	}
	var out bytes.Buffer
	if err := pageTemplate.Execute(&out, view); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func (b *builder) chapterIndex(c *chapter) int {
	for i, other := range b.chapters {
		if other == c {
			return i
		}
	}
	return -1
}

// tableOfContents collects level-2 and level-3 headings for the sidebar and
// omits the list when fewer than two entries exist.
func tableOfContents(doc *document) []tocEntry {
	var entries []tocEntry
	for _, h := range doc.headings {
		if h.level == 2 || h.level == 3 {
			entries = append(entries, tocEntry{Level: h.level, Text: h.text, ID: h.id})
		}
	}
	if len(entries) < 2 {
		return nil
	}
	return entries
}

func (b *builder) readingPath() []chapterView {
	var views []chapterView
	for _, c := range b.chapters {
		view := chapterView{Title: c.page.title, URL: b.pageURL(c.page), Rung: c.rung}
		for _, e := range c.experiments {
			target := view.URL
			if e.anchor != "" {
				target += "#" + e.anchor
			}
			view.Experiments = append(view.Experiments, experimentView{Title: e.heading, URL: target})
		}
		views = append(views, view)
	}
	return views
}

func (b *builder) reference() []navLink {
	var links []navLink
	if b.outline != nil {
		links = append(links, navLink{URL: b.pageURL(b.outline), Title: UILabels.Planned})
	}
	for _, p := range b.notes {
		links = append(links, navLink{URL: b.pageURL(p), Title: p.title})
	}
	return links
}
