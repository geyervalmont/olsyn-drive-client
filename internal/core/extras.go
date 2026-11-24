package core

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/geyervalmont/olsyn-drive-client/internal/agent"
	"github.com/geyervalmont/olsyn-drive-client/internal/driveapi"
	"github.com/geyervalmont/olsyn-drive-client/internal/journal"
)

// CacheUsage reports the running account's cache.
func (f *FS) CacheUsage(ctx context.Context) (agent.CacheUsage, error) {
	r := f.current()
	if r == nil {
		return agent.CacheUsage{}, nil
	}
	cs := r.cache.Stats()
	u := agent.CacheUsage{UsedBytes: cs.Used, CapBytes: cs.Cap}
	for _, q := range r.vfs.Queued() {
		if q.Kind == journal.OpUpload {
			u.UnsentFiles++
		}
	}
	for range r.vfs.Held() {
		u.UnsentFiles++
	}
	u.UnsentBytes = cs.Unsent
	return u, nil
}

// folderListMax bounds one page of folders; a folder of thousands of files
// is read through to find its subfolders.
const folderListMax = 500

// DriveFolders lists the subfolders of a drive folder for the send dialog.
func (f *FS) DriveFolders(ctx context.Context, parentPath, parentID, cursor string) (agent.DriveFolders, error) {
	r := f.current()
	if r == nil {
		return agent.DriveFolders{}, fmt.Errorf("core: not signed in")
	}
	var parent *driveapi.Node
	var err error
	switch {
	case parentID != "":
		var n *driveapi.NodeWithPath
		n, err = r.api.GetNode(ctx, parentID)
		if err == nil {
			parent, parentPath = &n.Node, r.vfs.PathOf(n.Node.ID)
			if parentPath == "" {
				parentPath = crumbsPath(n)
			}
		}
	default:
		parent, err = r.api.ResolvePath(ctx, parentPath)
	}
	if driveapi.IsNotFound(err) {
		return agent.DriveFolders{}, agent.ErrDriveFolderNotFound
	}
	if err != nil {
		return agent.DriveFolders{}, fmt.Errorf("find %s on the drive: %w", parentPath, mapDriveErr(err))
	}
	if !parent.IsDir() {
		return agent.DriveFolders{}, agent.ErrDriveFolderNotFound
	}
	can, _ := writable(parent)
	out := agent.DriveFolders{Parent: parentPath, ParentID: parent.ID, CanWrite: can, Folders: []agent.DriveFolder{}}
	for {
		page, err := r.api.Children(ctx, parent.ID, cursor, folderListMax)
		if err != nil {
			return agent.DriveFolders{}, fmt.Errorf("list %s: %w", parentPath, mapDriveErr(err))
		}
		for i := range page.Items {
			n := &page.Items[i]
			if !n.IsDir() || n.TrashedAt != nil {
				continue
			}
			ok, why := writable(n)
			out.Folders = append(out.Folders, agent.DriveFolder{ID: n.ID, Name: n.Name, Path: path.Join(parentPath, n.Name), CanWrite: ok, Why: why})
		}
		cursor = page.NextCursor
		if cursor == "" {
			return out, nil
		}
		if len(out.Folders) >= folderListMax {
			c := cursor
			out.NextCursor = &c
			return out, nil
		}
	}
}

// crumbsPath builds a drive path from a node's breadcrumbs: the folders
// above it, the drive's top first ("" when the top is not reachable).
func crumbsPath(n *driveapi.NodeWithPath) string {
	if n.Node.ParentID == "" {
		return "/"
	}
	names := make([]string, 0, len(n.Path))
	for i, c := range n.Path {
		if i == 0 {
			continue // the drive itself
		}
		names = append(names, c.Name)
	}
	return "/" + path.Join(append(names, n.Node.Name)...)
}

// writable says whether files can be added to a folder, and if not why.
func writable(n *driveapi.Node) (bool, string) {
	switch {
	case n.Sealed:
		return false, "sealed"
	case len(n.Managed) > 0 && strings.TrimSpace(string(n.Managed)) != "null" && !n.Can.Upload:
		return false, "managed"
	case !n.Can.Upload:
		return false, "read_only"
	}
	return true, ""
}

// Batches lists recent sends, newest first.
func (f *FS) Batches() []agent.Batch {
	if r := f.current(); r != nil {
		return r.sends.list()
	}
	return nil
}
