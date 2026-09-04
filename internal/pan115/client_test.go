package pan115

import (
	"strings"
	"testing"
)

func TestNormaliseCookie(t *testing.T) {
	for name, tc := range map[string]struct {
		raw     string
		want    string
		wantErr bool
	}{
		"canonical": {
			raw:  "UID=a; CID=b; SEID=c; KID=d",
			want: "UID=a; CID=b; SEID=c; KID=d",
		},
		"unspaced, order preserved": {
			raw:  "SEID=c;UID=a;KID=d;CID=b",
			want: "SEID=c; UID=a; KID=d; CID=b",
		},
		"lowercase names are normalised": {
			raw:  "uid=a; cid=b; seid=c; kid=d",
			want: "UID=a; CID=b; SEID=c; KID=d",
		},
		"pasted with a header name and other cookies": {
			// Everything is forwarded: the browser sends it all, and a
			// cookie 115 starts requiring later then needs no code change.
			raw:  "Cookie: _did=x; UID=a; CID=b; SEID=c; KID=d; acw_tc=y\n",
			want: "_did=x; UID=a; CID=b; SEID=c; KID=d; acw_tc=y",
		},
		"missing KID":    {raw: "UID=a; CID=b; SEID=c", wantErr: true},
		"missing SEID":   {raw: "UID=a; CID=b; KID=d", wantErr: true},
		"empty value":    {raw: "UID=; CID=b; SEID=c; KID=d", wantErr: true},
		"nothing at all": {raw: "   ", wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := normaliseCookie(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want an error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("normaliseCookie: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// Leaving KID out is the mistake that looks like an expired login: 115 answers
// 990001 rather than naming the missing cookie, so we name it first.
func TestMissingCookieNamesAreReported(t *testing.T) {
	_, err := normaliseCookie("UID=a; CID=b")
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"SEID", "KID"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention the missing %s", err, want)
		}
	}
}

func TestPageSizeIsClamped(t *testing.T) {
	client, err := New(Config{Cookie: "UID=a; CID=b; SEID=c; KID=d", PageSize: 99999})
	if err != nil {
		t.Fatal(err)
	}
	if client.pageSize != maxPageSize {
		t.Errorf("pageSize = %d, want %d", client.pageSize, maxPageSize)
	}
}

func TestEnvelopeErrors(t *testing.T) {
	if err := (envelope{State: true}).err(); err != nil {
		t.Errorf("a successful envelope produced %v", err)
	}
	if err := (envelope{Errno: 20004, Error: "busy"}).err(); err == nil {
		t.Error("a failed envelope produced no error")
	}
}
