package site

import (
	"bytes"
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

type heading struct {
	level int
	text  string
	id    string
	node  *ast.Heading
}

type document struct {
	source   []byte
	root     ast.Node
	title    string
	headings []heading
	ids      *slugger
}

// markdown configures goldmark with GitHub Flavored Markdown extensions and raw
// HTML passthrough so chapter markdown renders the same way it does on GitHub.
var markdown = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithRendererOptions(html.WithUnsafe()),
)

// parseMarkdown parses source into an AST, stamps GitHub-compatible id
// attributes onto every heading, and records the document's first H1 as title.
func parseMarkdown(source []byte) *document {
	doc := &document{source: source, ids: newSlugger()}
	doc.root = markdown.Parser().Parse(text.NewReader(source))
	ast.Walk(doc.root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		h, ok := n.(*ast.Heading)
		if !entering || !ok {
			return ast.WalkContinue, nil
		}
		label := plainText(h, source)
		id := doc.ids.slug(label)
		h.SetAttributeString("id", []byte(id))
		doc.headings = append(doc.headings, heading{level: h.Level, text: label, id: id, node: h})
		if h.Level == 1 && doc.title == "" {
			doc.title = label
		}
		return ast.WalkSkipChildren, nil
	})
	return doc
}

// plainText extracts visible heading text for slugging and TOC labels. Code
// spans stay literal while prose text decodes backslash escapes and HTML
// entities to match GitHub's heading anchor generation.
func plainText(n ast.Node, source []byte) string {
	var b strings.Builder
	ast.Walk(n, func(c ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch t := c.(type) {
		case *ast.CodeSpan:
			for child := t.FirstChild(); child != nil; child = child.NextSibling() {
				if segment, ok := child.(*ast.Text); ok {
					b.Write(segment.Segment.Value(source))
				}
			}
			return ast.WalkSkipChildren, nil
		case *ast.Text:
			value := util.UnescapePunctuations(t.Segment.Value(source))
			value = util.ResolveNumericReferences(value)
			b.Write(util.ResolveEntityNames(value))
			if t.SoftLineBreak() {
				b.WriteByte(' ')
			}
		case *ast.String:
			b.Write(t.Value)
		case *ast.RawHTML:
			return ast.WalkSkipChildren, nil
		}
		return ast.WalkContinue, nil
	})
	return strings.TrimSpace(b.String())
}

type slugger struct {
	occurrences map[string]int
}

func newSlugger() *slugger {
	return &slugger{occurrences: map[string]int{}}
}

// githubSlug reproduces github-slugger: lowercase, keep letters, digits,
// combining marks, underscores, and hyphens, replace spaces with hyphens, and
// drop all other punctuation.
func githubSlug(label string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(label) {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), unicode.IsMark(r), r == '_', r == '-':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		}
	}
	return b.String()
}

// slug assigns a unique anchor id for label within a document, falling back to
// "section" for punctuation-only headings and appending -1, -2, ... on repeats.
func (s *slugger) slug(label string) string {
	original := githubSlug(label)
	if original == "" {
		original = "section"
	}
	id := original
	for {
		if _, taken := s.occurrences[id]; !taken {
			break
		}
		s.occurrences[original]++
		id = fmt.Sprintf("%s-%d", original, s.occurrences[original])
	}
	s.occurrences[id] = 0
	return id
}

func newLink(dest, label string) *ast.Link {
	link := ast.NewLink()
	link.Destination = []byte(dest)
	link.AppendChild(link, ast.NewString([]byte(label)))
	return link
}

// addExperimentLinks inserts a "Code · Report · Evidence" paragraph directly after each
// experiment heading in a chapter using repo-relative paths that rewriteLinks
// later turns into GitHub URLs.
func (b *builder) addExperimentLinks(p *page) {
	if p.kind != chapterPage {
		return
	}
	byAnchor := map[string]experiment{}
	for _, e := range p.chapter.experiments {
		if e.anchor != "" && e.dir != "" {
			byAnchor[e.anchor] = e
		}
	}
	for _, h := range p.doc.headings {
		e, ok := byAnchor[h.id]
		if !ok {
			continue
		}
		para := ast.NewParagraph()
		para.SetAttributeString("class", []byte("experiment-links"))
		code := e.dir + "/main.go"
		evidence := "evidence/" + path.Base(e.dir) + ".txt"
		if b.files[p.chapter.dir+"/"+code] {
			para.AppendChild(para, newLink(code, UILabels.Code))
		}
		report := e.dir + "/report.sql"
		for _, link := range []struct{ file, label string }{{report, UILabels.Report}, {evidence, UILabels.Evidence}} {
			if !b.files[p.chapter.dir+"/"+link.file] {
				continue
			}
			if para.HasChildren() {
				para.AppendChild(para, ast.NewString([]byte(" · ")))
			}
			para.AppendChild(para, newLink(link.file, link.label))
		}
		if para.HasChildren() {
			h.node.Parent().InsertAfter(h.node.Parent(), h.node, para)
		}
	}
}

var includeMarkerPattern = regexp.MustCompile(`^<!--\s*include:\s*(\S+)(?:\s+lines=(\d+)-(\d+))?\s*-->\s*$`)

// rewriteLinks resolves every markdown link and image destination in p and
// replaces "<!-- include: path [lines=A-B] -->" markers with visible links to
// the inlined file on GitHub while leaving other HTML comments intact.
func (b *builder) rewriteLinks(p *page) {
	source := p.doc.source
	var includes []*ast.HTMLBlock
	ast.Walk(p.doc.root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch t := n.(type) {
		case *ast.Link:
			t.Destination = []byte(b.rewrite(p, string(t.Destination)))
		case *ast.Image:
			t.Destination = []byte(b.rewrite(p, string(t.Destination)))
		case *ast.HTMLBlock:
			includes = append(includes, t)
		}
		return ast.WalkContinue, nil
	})
	for _, block := range includes {
		var raw bytes.Buffer
		for i := 0; i < block.Lines().Len(); i++ {
			line := block.Lines().At(i)
			raw.Write(line.Value(source))
		}
		m := includeMarkerPattern.FindSubmatch(bytes.TrimSpace(raw.Bytes()))
		if m == nil {
			continue
		}
		dest := string(m[1])
		label := path.Clean(path.Join(path.Dir(p.source), dest))
		target := b.rewrite(p, dest)
		if m[2] != nil {
			label += ":" + string(m[2]) + "-" + string(m[3])
			target += "#L" + string(m[2]) + "-L" + string(m[3])
		}
		para := ast.NewParagraph()
		para.SetAttributeString("class", []byte("include"))
		para.AppendChild(para, newLink(target, label))
		block.Parent().ReplaceChild(block.Parent(), block, para)
	}
}

// proseOnly returns a copy of doc.source with code blocks, HTML blocks, and
// inline code spans overwritten by spaces so rung detection inspects prose only.
func proseOnly(doc *document) []byte {
	prose := bytes.Clone(doc.source)
	blank := func(start, stop int) {
		for i := start; i < stop; i++ {
			if prose[i] != '\n' {
				prose[i] = ' '
			}
		}
	}
	ast.Walk(doc.root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch t := n.(type) {
		case *ast.FencedCodeBlock, *ast.CodeBlock, *ast.HTMLBlock:
			lines := t.Lines()
			for i := 0; i < lines.Len(); i++ {
				blank(lines.At(i).Start, lines.At(i).Stop)
			}
			return ast.WalkSkipChildren, nil
		case *ast.CodeSpan:
			for child := t.FirstChild(); child != nil; child = child.NextSibling() {
				if segment, ok := child.(*ast.Text); ok {
					blank(segment.Segment.Start, segment.Segment.Stop)
				}
			}
			return ast.WalkSkipChildren, nil
		}
		return ast.WalkContinue, nil
	})
	return prose
}

func renderMarkdown(doc *document) ([]byte, error) {
	var out bytes.Buffer
	if err := markdown.Renderer().Render(&out, doc.source, doc.root); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
