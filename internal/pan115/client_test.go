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
		"reordered and unspaced": {
			raw:  "SEID=c;UID=a;CID=b",
			want: "UID=a; CID=b; SEID=c",
		},
		"pasted with header name and noise": {
			raw:  "Cookie: _did=x; UID=a; CID=b; SEID=c; acw_tc=y\n",
			want: "UID=a; CID=b; SEID=c",
		},
		"lowercase names": {
			raw:  "uid=a; cid=b; seid=c",
			want: "UID=a; CID=b; SEID=c",
		},
		"missing SEID":   {raw: "UID=a; CID=b", wantErr: true},
		"empty value":    {raw: "UID=; CID=b; SEID=c", wantErr: true},
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

func TestMissingCookieNamesAreReported(t *testing.T) {
	_, err := normaliseCookie("UID=a")
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"CID", "SEID"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention the missing %s", err, want)
		}
	}
}

func TestPageSizeIsClamped(t *testing.T) {
	client, err := New(Config{Cookie: "UID=a; CID=b; SEID=c", PageSize: 99999})
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
