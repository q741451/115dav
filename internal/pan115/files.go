package pan115

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// ErrNotFound reports that a directory id does not exist in the account.
var ErrNotFound = errors.New("115: no such directory")

// Entry is one child of a directory.
type Entry struct {
	// ID is the category id for a directory and the file id for a file.
	ID       string
	Name     string
	Size     int64
	IsDir    bool
	PickCode string
	SHA1     string
	ModTime  time.Time
}

// RootID is the category id of the account's top-level directory.
const RootID = "0"

// CheckAccess confirms the cookie is usable by listing the directory that is
// about to be served. An empty cid means the account root.
//
// Listing is also what will be done in anger, so this doubles as a smoke test
// of the exact code path -- worth more at startup than a dedicated session
// endpoint would be. Checking the mount point rather than the account root
// additionally catches a directory id that does not exist.
func (c *Client) CheckAccess(ctx context.Context, cid string) error {
	_, err := c.List(ctx, cid)
	return err
}

// maxListPages bounds the walk so that it terminates whatever 115 reports.
// At the default page size this is a million entries, which no directory
// reaches; the cap exists only so that a count which grows faster than the
// pages are served cannot spin here forever.
const maxListPages = 1000

// maxPreallocEntries bounds how much is reserved up front on 115's word alone.
// Beyond it the slice grows the ordinary way.
const maxPreallocEntries = 50000

// List returns every child of the directory with the given category id.
//
// It returns either the whole directory or an error. There is deliberately no
// third outcome: a partial listing is indistinguishable from a small one, and
// a media client that receives it concludes the library shrank and discards
// what it knew. Every way the endpoint can be short is therefore checked
// rather than accommodated.
func (c *Client) List(ctx context.Context, cid string) ([]Entry, error) {
	if cid == "" {
		cid = RootID
	}

	var entries []Entry
	for page := 0; page < maxListPages; page++ {
		got, err := c.listPage(ctx, cid, len(entries))
		if err != nil {
			return nil, err
		}
		if err := got.check(cid, len(entries)); err != nil {
			return nil, err
		}
		if entries == nil {
			// The first page says how many there will be, so the slice is
			// sized once instead of being grown a dozen times -- each growth
			// copying everything collected so far and leaving it as garbage.
			// The cap is against a count that is wrong rather than large:
			// being told there are ten million entries should not allocate
			// for ten million before the first one arrives.
			entries = make([]Entry, 0, min(got.Count.value, maxPreallocEntries))
		}

		for _, item := range got.Data {
			entries = append(entries, item.entry())
		}
		if len(entries) >= got.Count.value {
			return entries, nil
		}
	}
	return nil, fmt.Errorf("list %s: gave up after %d pages", cid, maxListPages)
}

// check rejects a page that cannot be true.
//
// The offset is where this page was expected to start, which is how many
// entries have been collected so far.
func (p *listResponse) check(cid string, offset int) error {
	// Asking for a directory that does not exist makes 115 quietly serve the
	// root instead, which would surface as the wrong listing.
	if got := string(p.CategoryID); got != cid {
		return fmt.Errorf("%w: asked for %s, got %s", ErrNotFound, cid, got)
	}
	if !p.Count.present {
		return fmt.Errorf("list %s: 115 did not say how many entries the directory holds", cid)
	}
	if p.Count.value < 0 {
		return fmt.Errorf("list %s: 115 reported a negative count (%d)", cid, p.Count.value)
	}
	// An empty page below the count is the shape both of a directory 115 has
	// decided not to serve and of a pagination walk that has lost its place.
	// Either way the entries are missing, and reporting what arrived would
	// present a truncated directory as a complete one.
	if len(p.Data) == 0 && offset < p.Count.value {
		return fmt.Errorf("list %s: 115 says the directory holds %d entries but served none from %d",
			cid, p.Count.value, offset)
	}
	return nil
}

func (c *Client) listPage(ctx context.Context, cid string, offset int) (*listResponse, error) {
	query := url.Values{
		"aid":              {"1"}, // 1 = live files, as opposed to the recycle bin
		"cid":              {cid},
		"o":                {"file_name"}, // stable across pages, unlike mtime
		"asc":              {"1"},
		"offset":           {strconv.Itoa(offset)},
		"limit":            {strconv.Itoa(c.pageSize)},
		"show_dir":         {"1"},
		"snap":             {"0"},
		"natsort":          {"0"},
		"record_open_time": {"0"}, // read-only: do not disturb the account
		"format":           {"json"},
		"fc_mix":           {"0"},
	}

	var page listResponse
	if err := c.get(ctx, c.listURL, query, &page); err != nil {
		return nil, fmt.Errorf("list %s: %w", cid, err)
	}
	return &page, nil
}

type listResponse struct {
	envelope
	CategoryID flexString `json:"cid"`
	// Count is how many entries the directory holds, and is what decides when
	// pagination stops -- hence optInt rather than int; see check.
	Count  optInt     `json:"count"`
	Offset int        `json:"offset"`
	Data   []listItem `json:"data"`
}

func (r *listResponse) status() envelope { return r.envelope }

// listItem mirrors the abbreviated field names the listing endpoint uses.
type listItem struct {
	FileID     string     `json:"fid"` // absent on directories
	CategoryID flexString `json:"cid"`
	ParentID   flexString `json:"pid"`
	Name       string     `json:"n"`
	Size       flexInt64  `json:"s"`
	SHA1       string     `json:"sha"`
	PickCode   string     `json:"pc"`
	Modified   flexString `json:"t"`
}

func (it listItem) entry() Entry {
	e := Entry{
		Name:     it.Name,
		Size:     int64(it.Size),
		PickCode: it.PickCode,
		SHA1:     it.SHA1,
		ModTime:  parseModTime(string(it.Modified)),
	}
	// A directory carries no file id; its own identity is the category id.
	if it.FileID == "" {
		e.IsDir = true
		e.ID = string(it.CategoryID)
		e.Size = 0
	} else {
		e.ID = it.FileID
	}
	return e
}

// chinaStandardTime is fixed: file timestamps come back as wall-clock strings
// with no zone, and China has not observed daylight saving since 1991.
var chinaStandardTime = time.FixedZone("CST", 8*60*60)

// parseModTime accepts both shapes the listing endpoint uses for "t": a Unix
// timestamp (directories) and a zone-less wall clock (files).
func parseModTime(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	if secs, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return time.Unix(secs, 0)
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(layout, raw, chinaStandardTime); err == nil {
			return t
		}
	}
	return time.Time{}
}
