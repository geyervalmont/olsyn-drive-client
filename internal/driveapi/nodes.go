package driveapi

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// NodeWithPath is GET /nodes/{id}: the node and its readable path.
type NodeWithPath struct {
	Node Node `json:"node"`
	Path []struct {
		ID   *string `json:"id"`
		Name string  `json:"name"`
	} `json:"path"`
}

// GetNode reads one node.
func (c *Client) GetNode(ctx context.Context, id string) (*NodeWithPath, error) {
	var out NodeWithPath
	err := c.do(ctx, request{op: "get node", method: http.MethodGet, path: pathID("/api/v1/nodes/", id, "")}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ChildrenMaxPage is the drive's largest children page.
const ChildrenMaxPage = 500

// Children reads one page of a folder's children (folders first, by name).
// Pass the previous page's NextCursor as cursor; "" starts at the top.
func (c *Client) Children(ctx context.Context, folderID, cursor string, limit int) (*Page[Node], error) {
	q := url.Values{"sort": {"name"}, "order": {"asc"}}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	if limit > 0 {
		if limit > ChildrenMaxPage {
			limit = ChildrenMaxPage
		}
		q.Set("limit", strconv.Itoa(limit))
	}
	var out Page[Node]
	err := c.do(ctx, request{op: "list children", method: http.MethodGet, path: pathID("/api/v1/nodes/", folderID, "/children"), query: q}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// AllChildren reads every page of a folder's children.
func (c *Client) AllChildren(ctx context.Context, folderID string) ([]Node, error) {
	var all []Node
	cursor := ""
	for {
		p, err := c.Children(ctx, folderID, cursor, ChildrenMaxPage)
		if err != nil {
			return nil, err
		}
		all = append(all, p.Items...)
		if p.NextCursor == "" {
			return all, nil
		}
		cursor = p.NextCursor
	}
}

// ResolvePath finds a node by its "/a/b/c" path.
func (c *Client) ResolvePath(ctx context.Context, path string) (*Node, error) {
	var out Node
	err := c.do(ctx, request{op: "resolve", method: http.MethodGet, path: "/api/v1/resolve", query: url.Values{"path": {path}}}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ResolveURI finds a node by its stable URI (https://…/n/<id>).
func (c *Client) ResolveURI(ctx context.Context, uri string) (*Node, error) {
	var out Node
	err := c.do(ctx, request{op: "resolve", method: http.MethodGet, path: "/api/v1/resolve", query: url.Values{"uri": {uri}}}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Conflict policies for creates and uploads.
const (
	ConflictFail       = "fail"
	ConflictRename     = "rename"
	ConflictNewVersion = "new_version"
)

// CreateFolder makes a folder. conflict is ConflictFail (409 name_conflict
// when taken) or ConflictRename. modifiedAt may be zero.
func (c *Client) CreateFolder(ctx context.Context, parentID, name, conflict string, modifiedAt time.Time) (*Node, error) {
	body := map[string]any{"name": name}
	if conflict != "" {
		body["conflict"] = conflict
	}
	if !modifiedAt.IsZero() {
		body["modified_at"] = FormatModifiedAt(modifiedAt)
	}
	var out Node
	err := c.do(ctx, request{op: "create folder", method: http.MethodPost, path: pathID("/api/v1/nodes/", parentID, "/folders"), body: body}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Patch changes a node's name, parent or client modified time. Nil fields
// are left alone; ClearModifiedAt sends modified_at: null.
type Patch struct {
	Name            *string
	ParentID        *string
	ModifiedAt      *time.Time
	ClearModifiedAt bool
}

// PatchNode renames, moves or sets the modified time of a node.
// Renames and moves have no precondition: the last one wins.
func (c *Client) PatchNode(ctx context.Context, nodeID string, p Patch) (*Node, error) {
	body := map[string]any{}
	if p.Name != nil {
		body["name"] = *p.Name
	}
	if p.ParentID != nil {
		body["parent_id"] = *p.ParentID
	}
	switch {
	case p.ClearModifiedAt:
		body["modified_at"] = nil
	case p.ModifiedAt != nil:
		body["modified_at"] = FormatModifiedAt(*p.ModifiedAt)
	}
	var out Node
	err := c.do(ctx, request{op: "patch node", method: http.MethodPatch, path: pathID("/api/v1/nodes/", nodeID, ""), body: body}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// TrashNode moves a node (and its subtree) to the drive's trash. The client
// never purges.
func (c *Client) TrashNode(ctx context.Context, nodeID string) error {
	return c.do(ctx, request{op: "trash node", method: http.MethodDelete, path: pathID("/api/v1/nodes/", nodeID, "")}, nil)
}
