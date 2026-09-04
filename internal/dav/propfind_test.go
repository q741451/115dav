package dav

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/q741451/115dav/internal/pan115"
)

// doBody is do with a request body, which PROPFIND needs and GET never does.
func doBody(t *testing.T, srv *httptest.Server, method, path string, headers map[string]string, payload string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestParsePropfind(t *testing.T) {
	for name, tc := range map[string]struct {
		body    string
		want    request
		wantErr bool
	}{
		// RFC 4918: a PROPFIND with no body means allprop. Clients send it
		// constantly, so treating it as malformed would break most of them.
		"empty body":      {body: "", want: request{allprop: true}},
		"whitespace only": {body: "\n  \n", want: request{allprop: true}},
		"allprop": {
			body: `<?xml version="1.0"?><D:propfind xmlns:D="DAV:"><D:allprop/></D:propfind>`,
			want: request{allprop: true},
		},
		"allprop with include": {
			body: `<D:propfind xmlns:D="DAV:"><D:allprop/>` +
				`<D:include><D:getetag/></D:include></D:propfind>`,
			want: request{allprop: true},
		},
		"propname": {
			body: `<D:propfind xmlns:D="DAV:"><D:propname/></D:propfind>`,
			want: request{propname: true},
		},
		"named properties": {
			body: `<D:propfind xmlns:D="DAV:"><D:prop>` +
				`<D:getcontentlength/><D:getetag/></D:prop></D:propfind>`,
			want: request{wanted: []xml.Name{
				{Space: "DAV:", Local: "getcontentlength"},
				{Space: "DAV:", Local: "getetag"},
			}},
		},
		// A client asking for something from its own namespace is normal; it
		// has to come back reported as absent rather than ignored.
		"foreign namespace": {
			body: `<D:propfind xmlns:D="DAV:" xmlns:A="http://apple.com/ns"><D:prop>` +
				`<A:quarantine/></D:prop></D:propfind>`,
			want: request{wanted: []xml.Name{{Space: "http://apple.com/ns", Local: "quarantine"}}},
		},
		"not xml":                   {body: "this is not xml", wantErr: true},
		"wrong root":                {body: `<D:lockinfo xmlns:D="DAV:"/>`, wantErr: true},
		"empty propfind":            {body: `<D:propfind xmlns:D="DAV:"></D:propfind>`, wantErr: true},
		"prop and allprop together": {body: `<D:propfind xmlns:D="DAV:"><D:allprop/><D:prop><D:getetag/></D:prop></D:propfind>`, wantErr: true},
		"truncated":                 {body: `<D:propfind xmlns:D="DAV:"><D:prop>`, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := parsePropfind(strings.NewReader(tc.body))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parsed %q as %+v, want an error", tc.body, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePropfind(%q): %v", tc.body, err)
			}
			if got.allprop != tc.want.allprop || got.propname != tc.want.propname {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
			if len(got.wanted) != len(tc.want.wanted) {
				t.Fatalf("got %d named properties, want %d", len(got.wanted), len(tc.want.wanted))
			}
			for i, name := range tc.want.wanted {
				if got.wanted[i] != name {
					t.Errorf("property %d = %v, want %v", i, got.wanted[i], name)
				}
			}
		})
	}
}

// The response is built from a snapshot taken before anything is written, so
// the number of entries in it is decided before the first byte goes out. A
// child cannot go missing from a 207 that still reports success -- which is
// exactly what the previous walker did when a listing failed part way.
func TestPropfindReportsEveryChildOrFails(t *testing.T) {
	b := sample(t)
	srv := newTestServer(t, b, Options{})

	resp := do(t, srv, "PROPFIND", "/", map[string]string{"Depth": "1"})
	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("status = %d, want 207", resp.StatusCode)
	}
	// The directory itself, plus every child.
	if got, want := strings.Count(body(t, resp), "<D:response>"), 1+len(b.dirs["0"]); got != want {
		t.Errorf("207 carried %d responses, want %d -- an entry went missing", got, want)
	}
}

func TestPropfindDepthZeroDescribesOnlyItself(t *testing.T) {
	b := sample(t)
	srv := newTestServer(t, b, Options{})

	resp := do(t, srv, "PROPFIND", "/", map[string]string{"Depth": "0"})
	if got := strings.Count(body(t, resp), "<D:response>"); got != 1 {
		t.Errorf("Depth 0 carried %d responses, want 1", got)
	}
}

// A property this mount cannot answer has to be reported as absent, in its own
// propstat, rather than left out of the response for the client to guess at.
func TestUnknownPropertiesAreReportedAsNotFound(t *testing.T) {
	b := sample(t)
	srv := newTestServer(t, b, Options{})

	req := `<D:propfind xmlns:D="DAV:" xmlns:A="http://apple.com/ns"><D:prop>` +
		`<D:getetag/><A:quarantine/></D:prop></D:propfind>`
	resp := doBody(t, srv, "PROPFIND", "/film.mkv", map[string]string{"Depth": "0"}, req)
	page := body(t, resp)

	if !strings.Contains(page, "HTTP/1.1 200 OK") || !strings.Contains(page, "HTTP/1.1 404 Not Found") {
		t.Errorf("want both a 200 and a 404 propstat, got:\n%s", page)
	}
	if !strings.Contains(page, "quarantine") {
		t.Errorf("the unanswerable property was not echoed back:\n%s", page)
	}
	// Only what was asked for.
	if strings.Contains(page, "getcontenttype") {
		t.Errorf("a property that was not requested was answered:\n%s", page)
	}
}

func TestPropnameAnswersNamesWithoutValues(t *testing.T) {
	b := sample(t)
	srv := newTestServer(t, b, Options{})

	req := `<D:propfind xmlns:D="DAV:"><D:propname/></D:propfind>`
	page := body(t, doBody(t, srv, "PROPFIND", "/film.mkv", map[string]string{"Depth": "0"}, req))

	if !strings.Contains(page, "<D:getetag/>") {
		t.Errorf("propname did not name getetag:\n%s", page)
	}
	if strings.Contains(page, "sha1:") {
		t.Errorf("propname answered a value as well as a name:\n%s", page)
	}
}

// Names come from 115 and reach the response as character data. A file called
// <b>&"x has to make well-formed XML, and has to come back out the way it went
// in -- a client that cannot parse the listing sees no library at all.
func TestNamesWithMarkupSurviveTheRoundTrip(t *testing.T) {
	nasty := `<b>&"' x.mkv`

	b := newFakeBackend(t)
	b.blobs["pc-nasty"] = []byte("payload")
	b.dirs["0"] = []pan115.Entry{{
		ID: "f1", Name: nasty, Size: 7, PickCode: "pc-nasty",
		SHA1: "abc", ModTime: time.Unix(1700000000, 0),
	}}
	srv := newTestServer(t, b, Options{})

	page := body(t, do(t, srv, "PROPFIND", "/", map[string]string{"Depth": "1"}))

	var parsed struct {
		Responses []struct {
			Href  string `xml:"href"`
			Props []struct {
				DisplayName string `xml:"displayname"`
				ETag        string `xml:"getetag"`
			} `xml:"propstat>prop"`
		} `xml:"response"`
	}
	if err := xml.Unmarshal([]byte(page), &parsed); err != nil {
		t.Fatalf("the response is not well-formed XML: %v\n%s", err, page)
	}
	if len(parsed.Responses) != 2 {
		t.Fatalf("got %d responses, want 2", len(parsed.Responses))
	}
	if got := parsed.Responses[1].Props[0].DisplayName; got != nasty {
		t.Errorf("displayname round-tripped as %q, want %q", got, nasty)
	}
	// The ETag is a quoted string by definition, and clients compare it with
	// the one in the header as text.
	if got := parsed.Responses[1].Props[0].ETag; got != `"sha1:abc"` {
		t.Errorf("getetag = %s, want %q", got, `"sha1:abc"`)
	}
}

// The href has to be a usable URL for the name it points at, or the client
// cannot fetch what the listing just told it about.
func TestHrefsAreUsable(t *testing.T) {
	b := sample(t)
	srv := newTestServer(t, b, Options{})

	page := body(t, do(t, srv, "PROPFIND", "/", map[string]string{"Depth": "1"}))
	var parsed struct {
		Responses []struct {
			Href string `xml:"href"`
		} `xml:"response"`
	}
	if err := xml.Unmarshal([]byte(page), &parsed); err != nil {
		t.Fatal(err)
	}

	var sawCollection bool
	for _, r := range parsed.Responses[1:] {
		resp := do(t, srv, http.MethodHead, r.Href, nil)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("HEAD %s = %d, want 200", r.Href, resp.StatusCode)
		}
		if strings.HasSuffix(r.Href, "/") {
			sawCollection = true
		}
	}
	if !sawCollection {
		t.Error("no directory href carried a trailing slash")
	}
}

func TestMalformedPropfindBodyIsRejected(t *testing.T) {
	b := sample(t)
	srv := newTestServer(t, b, Options{})

	resp := doBody(t, srv, "PROPFIND", "/", map[string]string{"Depth": "0"}, "<not-a-propfind/>")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestInvalidDepthIsRejected(t *testing.T) {
	b := sample(t)
	srv := newTestServer(t, b, Options{})

	resp := do(t, srv, "PROPFIND", "/", map[string]string{"Depth": "7"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}
