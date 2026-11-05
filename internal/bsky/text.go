package bsky

import (
	"context"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/rivo/uniseg"
)

// MaxGraphemes is Bluesky's post text limit.
const MaxGraphemes = 300

// Facet marks a byte range of the text as a link or a mention.
type Facet struct {
	Index    ByteSlice `json:"index"`
	Features []any     `json:"features"`
}

type ByteSlice struct {
	ByteStart int `json:"byteStart"`
	ByteEnd   int `json:"byteEnd"`
}

type LinkFeature struct {
	Type string `json:"$type"`
	URI  string `json:"uri"`
}

type MentionFeature struct {
	Type string `json:"$type"`
	DID  string `json:"did"`
}

var (
	linkRe    = regexp.MustCompile(`https?://[^\s<>"'）」』】、。]+`)
	mentionRe = regexp.MustCompile(`(^|[\s(（「])@([a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)+)`)
)

// Facets finds links and @handle mentions. resolve maps a handle to a DID; a handle it cannot
// resolve is left as plain text.
func Facets(ctx context.Context, text string, resolve func(context.Context, string) (string, error)) []Facet {
	var out []Facet
	for _, m := range linkRe.FindAllStringIndex(text, -1) {
		uri := strings.TrimRight(text[m[0]:m[1]], ".,!?:;)")
		end := m[0] + len(uri)
		out = append(out, Facet{
			Index:    ByteSlice{m[0], end},
			Features: []any{LinkFeature{Type: "app.bsky.richtext.facet#link", URI: uri}},
		})
	}
	if resolve != nil {
		for _, m := range mentionRe.FindAllStringSubmatchIndex(text, -1) {
			start, end := m[4]-1, m[5] // include the '@'
			handle := text[m[4]:m[5]]
			did, err := resolve(ctx, handle)
			if err != nil || did == "" {
				continue
			}
			out = append(out, Facet{
				Index:    ByteSlice{start, end},
				Features: []any{MentionFeature{Type: "app.bsky.richtext.facet#mention", DID: did}},
			})
		}
	}
	return out
}

// Graphemes counts user-perceived characters, the unit of Bluesky's limit.
func Graphemes(s string) int { return uniseg.GraphemeClusterCount(s) }

// graphemeOffset is the byte offset after the first n graphemes (len(s) if s is shorter).
func graphemeOffset(s string, n int) int {
	end := 0
	g := uniseg.NewGraphemes(s)
	for i := 0; i < n && g.Next(); i++ {
		_, end = g.Positions()
	}
	return end
}

// CutGraphemes is the first n graphemes of s.
func CutGraphemes(s string, n int) string { return s[:graphemeOffset(s, n)] }

// SplitThread cuts text into parts of at most limit graphemes, preferring line breaks, then
// sentence ends, then spaces. Links are never cut.
func SplitThread(text string, limit int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	var parts []string
	for Graphemes(text) > limit {
		cut := cutPoint(text, limit)
		part := strings.TrimSpace(text[:cut])
		if part != "" {
			parts = append(parts, part)
		}
		text = strings.TrimSpace(text[cut:])
	}
	if text != "" {
		parts = append(parts, text)
	}
	return parts
}

// cutPoint is a byte offset where the first part (≤ limit graphemes) ends.
func cutPoint(text string, limit int) int {
	hard := graphemeOffset(text, limit)
	window := text[:hard]
	// do not end inside a link
	for _, m := range linkRe.FindAllStringIndex(text, -1) {
		if m[0] < hard && m[1] > hard && m[0] > 0 {
			hard = m[0]
			window = text[:hard]
		}
	}
	best := -1
	for _, sep := range []string{"\n\n", "\n"} {
		if i := strings.LastIndex(window, sep); i > len(window)/3 {
			return i + len(sep)
		}
	}
	for _, sep := range []string{"。", "！", "？", ". ", "! ", "? "} {
		if i := strings.LastIndex(window, sep); i > best {
			best = i + len(sep)
		}
	}
	if best > len(window)/3 {
		return best
	}
	if i := strings.LastIndexAny(window, " 　、,"); i > len(window)/3 {
		_, size := utf8.DecodeRuneInString(window[i:])
		return i + size
	}
	if hard == 0 {
		return len(text)
	}
	return hard
}
