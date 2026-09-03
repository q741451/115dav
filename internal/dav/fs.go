package dav

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"

	"golang.org/x/net/webdav"

	"github.com/q741451/115dav/internal/pan115"
)

// fileSystem adapts Tree to the interface x/net/webdav walks during PROPFIND.
//
// Only metadata is served here. Reading bytes is handled by the streamer,
// which can pass a range request straight through to the CDN instead of
// emulating a seekable file.
type fileSystem struct{ tree *Tree }

var _ webdav.FileSystem = (*fileSystem)(nil)

// The mount is read-only, so every mutating entry point is refused.

func (f *fileSystem) Mkdir(context.Context, string, os.FileMode) error { return os.ErrPermission }
func (f *fileSystem) RemoveAll(context.Context, string) error          { return os.ErrPermission }
func (f *fileSystem) Rename(context.Context, string, string) error     { return os.ErrPermission }

func (f *fileSystem) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	entry, err := f.tree.Lookup(ctx, name)
	if err != nil {
		return nil, toPathError("stat", name, err)
	}
	return fileInfo{entry}, nil
}

func (f *fileSystem) OpenFile(ctx context.Context, name string, flag int, _ os.FileMode) (webdav.File, error) {
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_APPEND|os.O_TRUNC) != 0 {
		return nil, &fs.PathError{Op: "open", Path: name, Err: os.ErrPermission}
	}
	entry, err := f.tree.Lookup(ctx, name)
	if err != nil {
		return nil, toPathError("open", name, err)
	}
	return &node{tree: f.tree, entry: entry}, nil
}

func toPathError(op, name string, err error) error {
	return &fs.PathError{Op: op, Path: name, Err: err}
}

// node is an open handle. It answers metadata questions and, for directories,
// enumerates children.
type node struct {
	tree  *Tree
	entry pan115.Entry

	// listing is populated on first Readdir and consumed across calls.
	listing []os.FileInfo
	offset  int
}

var _ webdav.File = (*node)(nil)

func (n *node) Stat() (os.FileInfo, error) { return fileInfo{n.entry}, nil }
func (n *node) Close() error               { return nil }

func (n *node) Write([]byte) (int, error) {
	return 0, &fs.PathError{Op: "write", Path: n.entry.Name, Err: os.ErrPermission}
}

// Read and Seek exist only to satisfy the interface. GET never reaches here:
// the server routes it to the streamer before the WebDAV handler sees it.
func (n *node) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: n.entry.Name, Err: fs.ErrInvalid}
}

func (n *node) Seek(int64, int) (int64, error) {
	return 0, &fs.PathError{Op: "seek", Path: n.entry.Name, Err: fs.ErrInvalid}
}

func (n *node) Readdir(count int) ([]os.FileInfo, error) {
	if !n.entry.IsDir {
		return nil, &fs.PathError{Op: "readdir", Path: n.entry.Name, Err: fs.ErrInvalid}
	}
	if n.listing == nil {
		entries, err := n.tree.Children(context.Background(), n.entry.ID)
		if err != nil {
			return nil, toPathError("readdir", n.entry.Name, err)
		}
		n.listing = make([]os.FileInfo, len(entries))
		for i, entry := range entries {
			n.listing[i] = fileInfo{entry}
		}
	}

	remaining := n.listing[n.offset:]
	if count <= 0 {
		n.offset = len(n.listing)
		return remaining, nil
	}
	if len(remaining) == 0 {
		return nil, io.EOF
	}
	n.offset += min(count, len(remaining))
	return remaining[:min(count, len(remaining))], nil
}

// fileInfo exposes a 115 entry as an os.FileInfo.
//
// The WebDAV property helpers look for ETager and ContentTyper here rather
// than on the open file, so both live on this type.
type fileInfo struct{ entry pan115.Entry }

var (
	_ os.FileInfo         = fileInfo{}
	_ webdav.ETager       = fileInfo{}
	_ webdav.ContentTyper = fileInfo{}
)

// ContentType lets PROPFIND report a useful getcontenttype, which is how a
// media client decides a file is worth looking at.
//
// Answering here also keeps the fallback path out of play: it would sniff the
// first bytes of the file, which for this filesystem means a pointless CDN
// round trip at best and an error at worst.
func (fi fileInfo) ContentType(context.Context) (string, error) {
	if fi.entry.IsDir {
		return "httpd/unix-directory", nil
	}
	return contentType(fi.entry.Name), nil
}

// ETag prefers the content hash 115 already stores, which stays stable across
// restarts and re-listings, unlike one derived from mtime and size.
func (fi fileInfo) ETag(context.Context) (string, error) {
	if fi.entry.SHA1 != "" {
		return fmt.Sprintf("%q", "sha1:"+fi.entry.SHA1), nil
	}
	return fmt.Sprintf(`"%x%x"`, fi.ModTime().UnixNano(), fi.Size()), nil
}

func (fi fileInfo) Name() string { return fi.entry.Name }
func (fi fileInfo) IsDir() bool  { return fi.entry.IsDir }
func (fi fileInfo) Sys() any     { return fi.entry }

func (fi fileInfo) Size() int64 {
	if fi.entry.IsDir {
		return 0
	}
	return fi.entry.Size
}

func (fi fileInfo) Mode() os.FileMode {
	if fi.entry.IsDir {
		return os.ModeDir | 0o555
	}
	return 0o444
}

func (fi fileInfo) ModTime() time.Time {
	if fi.entry.ModTime.IsZero() {
		// A zero time serialises as year 1, which some clients reject.
		return time.Unix(0, 0)
	}
	return fi.entry.ModTime
}
