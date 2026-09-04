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

// List returns every child of the directory with the given category id.
func (c *Client) List(ctx context.Context, cid string) ([]Entry, error) {
	if cid == "" {
		cid = RootID
	}

	var (
		entries []Entry
		offset  int
	)
	for {
		page, err := c.listPage(ctx, cid, offset)
		if err != nil {
			return nil, err
		}

		// Asking for a directory that does not exist makes 115 quietly serve
		// the root instead, which would surface as the wrong listing.
		if got := string(page.CategoryID); got != cid {
			return nil, fmt.Errorf("%w: asked for %s, got %s", ErrNotFound, cid, got)
		}

		for _, item := range page.Data {
			entries = append(entries, item.entry())
		}

		// Stop on a short or empty page rather than trusting Count alone: a
		// count that disagrees with what the endpoint will actually hand over
		// would otherwise spin here forever.
		if len(page.Data) == 0 || len(entries) >= page.Count {
			return entries, nil
		}
		offset += len(page.Data)
	}
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
	Count      int        `json:"count"`
	Offset     int        `json:"offset"`
	Data       []listItem `json:"data"`
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
