package dav

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/q741451/115dav/internal/pan115"
)

// PROPFIND is answered in two halves that cannot interleave: everything the
// response will mention is collected first, and only then is any of it
// written.
//
// The split is the whole point. x/net/webdav walks the filesystem while it
// serialises, calling Stat and OpenFile for every child, so a listing that
// expires or fails part way through arrives after the 207 status line has
// gone out -- and a failure there is not reportable. Worse, its walker treats
// a *fs.PathError, which is every error this package can produce, as "skip
// this one", so the child silently vanishes from a response that still claims
// success. A directory that lost half its files looks exactly like a directory
// that has half as many.
//
// Gathering first means every failure happens while a status code is still
// available, and rendering cannot fail for want of data it does not have.

// snapshot is everything one PROPFIND will report.
type snapshot struct {
	// base is the cleaned request path, always rooted and without a trailing
	// slash except at the root itself.
	base string
	self pan115.Entry
	// children is nil unless the request asked for depth 1 on a directory.
	children []pan115.Entry
}

// snapshot resolves the whole response up front.
func (e *epoch) snapshot(ctx context.Context, name string, depth int) (snapshot, error) {
	entry, err := e.tree.Lookup(ctx, name)
	if err != nil {
		return snapshot{}, err
	}

	snap := snapshot{base: path.Clean("/" + strings.Trim(name, "/")), self: entry}
	if depth == 1 && entry.IsDir {
		if snap.children, err = e.tree.Children(ctx, entry.ID); err != nil {
			return snapshot{}, err
		}
	}
	return snap, nil
}

// href is the URL a response element points at. Directories carry a trailing
// slash, which is how a client tells a collection from a resource before it
// has read resourcetype.
func (s snapshot) href(entry pan115.Entry, isSelf bool) string {
	p := s.base
	if !isSelf {
		p = path.Join(s.base, entry.Name)
	}
	if entry.IsDir && p != "/" {
		p += "/"
	}
	return (&url.URL{Path: p}).EscapedPath()
}

// liveProperty is one property this mount can answer.
type liveProperty struct {
	name string // local name, in the DAV: namespace
	// onDir is false for the properties that describe file content, which a
	// collection does not have.
	onDir bool
	value func(pan115.Entry) string
	// raw marks a value that is already XML rather than text to be escaped.
	raw bool
}

// liveProps is everything this mount publishes, in a fixed order so that two
// runs produce the same bytes.
//
// It is deliberately the same set x/net/webdav answered, so that replacing it
// changes how the response is produced and not what it says. The omissions are
// its omissions too: creationdate and getcontentlanguage because 115 reports
// neither, and lockdiscovery because nothing here can hold a lock.
var liveProps = []liveProperty{
	{name: "resourcetype", onDir: true, raw: true, value: func(e pan115.Entry) string {
		if e.IsDir {
			return `<D:collection/>`
		}
		return ""
	}},
	{name: "displayname", onDir: true, value: func(e pan115.Entry) string {
		// The root's real name is the mount point's business, not a client's.
		if e.Name == "/" {
			return ""
		}
		return e.Name
	}},
	{name: "getcontentlength", value: func(e pan115.Entry) string {
		return strconv.FormatInt(e.Size, 10)
	}},
	{name: "getlastmodified", onDir: true, value: func(e pan115.Entry) string {
		return modTimeOf(e).UTC().Format(http.TimeFormat)
	}},
	{name: "getcontenttype", value: func(e pan115.Entry) string {
		return contentType(e.Name)
	}},
	{name: "getetag", value: func(e pan115.Entry) string {
		return entryTag(e)
	}},
	{name: "supportedlock", onDir: true, raw: true, value: func(pan115.Entry) string {
		// Empty: no lock type is supported, which is what RFC 4918 says an
		// empty supportedlock means and what is actually true here. LOCK is
		// answered 405, and OPTIONS advertises DAV class 1, which does not
		// include locking -- so the exclusive write lock this used to claim
		// contradicted both.
		//
		// The property is still reported rather than omitted. It is a live
		// property, and answering "none" is a different statement from having
		// no answer.
		return ""
	}},
}

func (p liveProperty) appliesTo(entry pan115.Entry) bool {
	return p.onDir || !entry.IsDir
}

// request is a parsed PROPFIND body.
type request struct {
	// allprop is set by <allprop/> and by an empty body, which RFC 4918 says
	// means the same thing.
	allprop bool
	// propname asks for the names alone, with no values.
	propname bool
	// wanted is the explicit <prop> list, in the order the client asked.
	wanted []xml.Name
}

// errInvalidPropfind is answered with 400; the body was not a PROPFIND.
var errInvalidPropfind = fmt.Errorf("dav: invalid PROPFIND request body")

// parsePropfind reads the request body.
//
// maxPropfindBody bounds it: the body is a short list of property names, and
// nothing else should be arriving on this method.
const maxPropfindBody = 1 << 20

func parsePropfind(r io.Reader) (request, error) {
	var body struct {
		XMLName  xml.Name   `xml:"DAV: propfind"`
		Allprop  *struct{}  `xml:"DAV: allprop"`
		Propname *struct{}  `xml:"DAV: propname"`
		Prop     *propNames `xml:"DAV: prop"`
		Include  *propNames `xml:"DAV: include"`
	}

	raw, err := io.ReadAll(io.LimitReader(r, maxPropfindBody))
	if err != nil {
		return request{}, err
	}
	// An empty body means allprop. Clients send it constantly.
	if len(strings.TrimSpace(string(raw))) == 0 {
		return request{allprop: true}, nil
	}
	if err := xml.Unmarshal(raw, &body); err != nil {
		return request{}, errInvalidPropfind
	}

	switch {
	case body.Prop != nil:
		if body.Allprop != nil || body.Propname != nil {
			return request{}, errInvalidPropfind
		}
		return request{wanted: body.Prop.Names}, nil
	case body.Propname != nil:
		if body.Allprop != nil {
			return request{}, errInvalidPropfind
		}
		return request{propname: true}, nil
	case body.Allprop != nil:
		// <include> names properties a server might otherwise leave out of
		// allprop. This one leaves nothing out, so they are already covered.
		return request{allprop: true}, nil
	}
	return request{}, errInvalidPropfind
}

// propNames collects the child element names of <prop> or <include>.
type propNames struct{ Names []xml.Name }

func (p *propNames) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	for {
		token, err := d.Token()
		if err != nil {
			return err
		}
		switch t := token.(type) {
		case xml.StartElement:
			p.Names = append(p.Names, t.Name)
			if err := d.Skip(); err != nil {
				return err
			}
		case xml.EndElement:
			return nil
		}
	}
}

// writeMultistatus renders the snapshot. It performs no I/O of its own and
// cannot fail for anything but the client hanging up, which is what makes a
// half-written 207 impossible.
func writeMultistatus(w io.Writer, snap snapshot, req request) error {
	var page strings.Builder
	page.WriteString(xml.Header)
	page.WriteString(`<D:multistatus xmlns:D="DAV:">`)

	writeResponse(&page, snap.href(snap.self, true), snap.self, req)
	for _, child := range snap.children {
		writeResponse(&page, snap.href(child, false), child, req)
	}

	page.WriteString(`</D:multistatus>`)
	_, err := io.WriteString(w, page.String())
	return err
}

func writeResponse(page *strings.Builder, href string, entry pan115.Entry, req request) {
	page.WriteString(`<D:response><D:href>`)
	escape(page, href)
	page.WriteString(`</D:href>`)

	found, missing := selectProps(entry, req)

	if len(found) > 0 {
		page.WriteString(`<D:propstat><D:prop>`)
		for _, prop := range found {
			writeProp(page, entry, prop, req.propname)
		}
		page.WriteString(`</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat>`)
	}
	if len(missing) > 0 {
		// Asked for by name and not answerable here. RFC 4918 wants these
		// reported rather than omitted, in a propstat of their own.
		page.WriteString(`<D:propstat><D:prop>`)
		for _, name := range missing {
			writeUnknownProp(page, name)
		}
		page.WriteString(`</D:prop><D:status>HTTP/1.1 404 Not Found</D:status></D:propstat>`)
	}
	page.WriteString(`</D:response>`)
}

// selectProps decides what this entry answers, and what it cannot.
func selectProps(entry pan115.Entry, req request) (found []liveProperty, missing []xml.Name) {
	if req.allprop || req.propname {
		for _, prop := range liveProps {
			if prop.appliesTo(entry) {
				found = append(found, prop)
			}
		}
		return found, nil
	}
	for _, name := range req.wanted {
		prop, ok := lookupProp(name)
		if ok && prop.appliesTo(entry) {
			found = append(found, prop)
			continue
		}
		missing = append(missing, name)
	}
	return found, missing
}

func lookupProp(name xml.Name) (liveProperty, bool) {
	if name.Space != "DAV:" {
		return liveProperty{}, false
	}
	for _, prop := range liveProps {
		if prop.name == name.Local {
			return prop, true
		}
	}
	return liveProperty{}, false
}

func writeProp(page *strings.Builder, entry pan115.Entry, prop liveProperty, namesOnly bool) {
	if namesOnly {
		page.WriteString("<D:" + prop.name + "/>")
		return
	}
	value := prop.value(entry)
	if value == "" {
		page.WriteString("<D:" + prop.name + "/>")
		return
	}
	page.WriteString("<D:" + prop.name + ">")
	if prop.raw {
		page.WriteString(value)
	} else {
		escape(page, value)
	}
	page.WriteString("</D:" + prop.name + ">")
}

// writeUnknownProp echoes a name the client asked for, keeping its namespace
// so the client can match the answer to its question.
func writeUnknownProp(page *strings.Builder, name xml.Name) {
	if name.Space == "DAV:" {
		page.WriteString("<D:" + xmlName(name.Local) + "/>")
		return
	}
	page.WriteString(`<x:` + xmlName(name.Local) + ` xmlns:x="`)
	escape(page, name.Space)
	page.WriteString(`"/>`)
}

// xmlName keeps a name the client made up from becoming markup. Anything that
// cannot be an element name is dropped rather than escaped, because there is
// no escaping inside a tag.
func xmlName(local string) string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_', r == '.':
			return r
		}
		return -1
	}, local)
	if clean == "" {
		return "unknown"
	}
	return clean
}

// escape writes s as XML character data, escaping what XML requires and
// nothing else.
//
// encoding/xml's EscapeText additionally rewrites a quote as &#34;. That is
// legal, and a conformant parser resolves it back, but an ETag is a quoted
// string by definition -- every tag this server publishes would come out
// looking different from the one it puts in the ETag header, and clients that
// compare the two as strings would stop matching them.
//
// A rune XML 1.0 cannot represent at all is replaced rather than escaped;
// there is no character reference for it. sanitiseName has already taken the
// control characters out of any name that reaches here, so this is the second
// line of a defence rather than the first.
func escape(page *strings.Builder, s string) {
	for _, r := range s {
		switch {
		case r == '&':
			page.WriteString("&amp;")
		case r == '<':
			page.WriteString("&lt;")
		case r == '>':
			page.WriteString("&gt;")
		case r == '\r':
			// Otherwise a parser folds it into a newline and the value that
			// comes back out is not the one that went in.
			page.WriteString("&#xD;")
		case isXMLChar(r):
			page.WriteRune(r)
		default:
			page.WriteRune(unicode.ReplacementChar)
		}
	}
}

// isXMLChar reports whether a rune is one XML 1.0 permits in content.
func isXMLChar(r rune) bool {
	return r == '\t' || r == '\n' || r == '\r' ||
		(r >= 0x20 && r <= 0xD7FF) ||
		(r >= 0xE000 && r <= 0xFFFD) ||
		(r >= 0x10000 && r <= 0x10FFFF)
}

// modTimeOf substitutes the epoch for a file 115 gave no timestamp for: a zero
// time serialises as year 1, which some clients reject outright.
func modTimeOf(entry pan115.Entry) time.Time {
	if entry.ModTime.IsZero() {
		return time.Unix(0, 0)
	}
	return entry.ModTime
}
