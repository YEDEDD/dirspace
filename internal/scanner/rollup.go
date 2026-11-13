package scanner

import (
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/YEDEDD/dirspace/internal/model"
)

// rollupCollector keeps subtree aggregates for the depth-limited directory
// index. It only keeps levels up to depth; deeper tasks contribute their direct
// bytes to those ancestors, so an indexed directory is still complete even when
// the tree below it is much deeper than the index.
type rollupCollector struct {
	rootPath string
	depth    int
	topK     int
	all      bool
	accuracy model.Accuracy
	status   model.DataStatus

	mu    sync.Mutex
	nodes map[string]*rollupNode
}

type rollupNode struct {
	path      string
	agg       treeAggregate
	truncated bool
}

func newRollupCollector(rootPath string, depth, topK int, accuracy model.Accuracy, all bool) *rollupCollector {
	if !all && (depth <= 0 || topK <= 0) {
		return nil
	}
	return &rollupCollector{
		rootPath: rootPath,
		depth:    depth,
		topK:     topK,
		all:      all,
		accuracy: accuracy,
		status:   model.DataFresh,
		nodes:    make(map[string]*rollupNode),
	}
}

// relPath converts an absolute scanned path to a slash-separated root-relative
// path. The scan root itself has no index row.
func (c *rollupCollector) relPath(abs string) (string, int, bool) {
	rel, err := filepath.Rel(c.rootPath, abs)
	if err != nil || rel == "." || rel == "" {
		return "", 0, false
	}
	rel = filepath.ToSlash(rel)
	if strings.HasPrefix(rel, "../") || rel == ".." {
		return "", 0, false
	}
	depth := strings.Count(rel, "/") + 1
	return rel, depth, true
}

// addDirect records direct statistics for one directory. It adds them to the
// directory and to every indexed ancestor. This is used by the parallel exact
// walker, where a task returns only the directory's own bytes and files.
func (c *rollupCollector) addDirect(abs string, agg treeAggregate, truncated bool) {
	if c == nil {
		return
	}
	path, depth, ok := c.relPath(abs)
	if !ok {
		return
	}
	_ = depth
	c.mu.Lock()
	defer c.mu.Unlock()
	for p := path; p != ""; p = parentIndexPath(p) {
		level := strings.Count(p, "/") + 1
		if !c.all && level > c.depth {
			continue
		}
		n := c.nodeLocked(p)
		n.agg.add(agg)
		if truncated {
			n.truncated = true
		}
	}
}

// recordSubtree records a fully aggregated subtree. This is used by the quick
// recursive walker, which already has child totals when a directory returns.
func (c *rollupCollector) recordSubtree(abs string, agg treeAggregate) {
	if c == nil {
		return
	}
	path, depth, ok := c.relPath(abs)
	if !ok || (!c.all && depth > c.depth) {
		return
	}
	c.mu.Lock()
	c.nodeLocked(path).agg.add(agg)
	c.mu.Unlock()
}

func (c *rollupCollector) nodeLocked(path string) *rollupNode {
	n := c.nodes[path]
	if n == nil {
		n = &rollupNode{path: path}
		c.nodes[path] = n
	}
	return n
}

func parentIndexPath(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return ""
}

// liveRows returns a bounded snapshot of completed directories for the active
// run. Unlike rows, it always applies depth/topK and marks the rows partial;
// it is safe to call while scanning continues.
const liveIndexRowCap = 100000

func (c *rollupCollector) liveRows(maxDepth, topK int, now time.Time) []model.DirIndexEntry {
	if c == nil {
		return nil
	}
	if maxDepth < 1 {
		maxDepth = 1
	}
	if topK < 1 {
		topK = 1
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	children := make(map[string][]string)
	for path := range c.nodes {
		depth := strings.Count(path, "/") + 1
		if depth > maxDepth {
			continue
		}
		children[parentIndexPath(path)] = append(children[parentIndexPath(path)], path)
	}
	for parent := range children {
		sort.Slice(children[parent], func(i, j int) bool {
			a, b := c.nodes[children[parent][i]], c.nodes[children[parent][j]]
			if a.agg.allocBytes != b.agg.allocBytes {
				return a.agg.allocBytes > b.agg.allocBytes
			}
			return a.path < b.path
		})
	}

	selected := make(map[string]bool)
	for _, path := range children[""] {
		if len(selected) >= liveIndexRowCap {
			break
		}
		selected[path] = true
	}
	for depth := 1; depth < maxDepth; depth++ {
		var parents []string
		for path := range selected {
			if strings.Count(path, "/")+1 == depth {
				parents = append(parents, path)
			}
		}
		sort.Strings(parents)
		for _, parent := range parents {
			if len(selected) >= liveIndexRowCap {
				break
			}
			kids := children[parent]
			if len(kids) > topK {
				kids = kids[:topK]
			}
			for _, child := range kids {
				if len(selected) >= liveIndexRowCap {
					break
				}
				selected[child] = true
			}
		}
	}

	result := make([]model.DirIndexEntry, 0, len(selected))
	for path := range selected {
		n := c.nodes[path]
		if n == nil || strings.Count(path, "/")+1 > maxDepth {
			continue
		}
		name := path
		if i := strings.LastIndexByte(path, '/'); i >= 0 {
			name = path[i+1:]
		}
		if len(result) >= liveIndexRowCap {
			break
		}
		result = append(result, model.DirIndexEntry{
			Path: path, ParentPath: parentIndexPath(path), Name: name,
			Depth:      strings.Count(path, "/") + 1,
			AllocBytes: n.agg.allocBytes, LogicalBytes: n.agg.logicalBytes,
			FileCount: n.agg.fileCount, DirCount: n.agg.dirCount,
			SymlinkCount: n.agg.symlinkCount, DataStatus: model.DataPartial,
			Accuracy: c.accuracy, Truncated: n.truncated, UpdatedAt: now,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Depth != result[j].Depth {
			return result[i].Depth < result[j].Depth
		}
		return result[i].Path < result[j].Path
	})
	return result
}

// rows selects the top-K children of every indexed parent. Top-level entries
// are always retained; descendants are retained only when their parent was
// retained. This keeps the index bounded (topK^depth) without storing every
// directory in a multi-billion-entry filesystem.
func (c *rollupCollector) rows(now time.Time) []model.DirIndexEntry {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	children := make(map[string][]string)
	for path := range c.nodes {
		children[parentIndexPath(path)] = append(children[parentIndexPath(path)], path)
	}
	for parent := range children {
		sort.Slice(children[parent], func(i, j int) bool {
			a, b := c.nodes[children[parent][i]], c.nodes[children[parent][j]]
			if a.agg.allocBytes != b.agg.allocBytes {
				return a.agg.allocBytes > b.agg.allocBytes
			}
			return a.path < b.path
		})
	}

	if c.all {
		result := make([]model.DirIndexEntry, 0, len(c.nodes))
		for path, n := range c.nodes {
			if n == nil {
				continue
			}
			parent := parentIndexPath(path)
			name := path
			if i := strings.LastIndexByte(path, '/'); i >= 0 {
				name = path[i+1:]
			}
			if len(result) >= liveIndexRowCap {
				break
			}
			result = append(result, model.DirIndexEntry{
				Path:         path,
				ParentPath:   parent,
				Name:         name,
				Depth:        strings.Count(path, "/") + 1,
				AllocBytes:   n.agg.allocBytes,
				LogicalBytes: n.agg.logicalBytes,
				FileCount:    n.agg.fileCount,
				DirCount:     n.agg.dirCount,
				SymlinkCount: n.agg.symlinkCount,
				DataStatus:   c.status,
				Accuracy:     c.accuracy,
				Truncated:    n.truncated,
				UpdatedAt:    now,
			})
		}
		sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
		return result
	}

	selected := make(map[string]bool)
	// Top-level entries are all retained; this matches top_dirs.
	for _, path := range children[""] {
		selected[path] = true
	}
	// Select top-K children for each retained parent, level by level.
	for depth := 1; depth < c.depth; depth++ {
		var parents []string
		for path := range selected {
			if strings.Count(path, "/")+1 == depth {
				parents = append(parents, path)
			}
		}
		sort.Strings(parents)
		for _, parent := range parents {
			kids := children[parent]
			if len(kids) > c.topK {
				kids = kids[:c.topK]
			}
			for _, child := range kids {
				selected[child] = true
			}
		}
	}

	result := make([]model.DirIndexEntry, 0, len(selected))
	for path := range selected {
		n := c.nodes[path]
		if n == nil {
			continue
		}
		parent := parentIndexPath(path)
		name := path
		if i := strings.LastIndexByte(path, '/'); i >= 0 {
			name = path[i+1:]
		}
		if len(result) >= liveIndexRowCap {
			break
		}
		result = append(result, model.DirIndexEntry{
			Path:         path,
			ParentPath:   parent,
			Name:         name,
			Depth:        strings.Count(path, "/") + 1,
			AllocBytes:   n.agg.allocBytes,
			LogicalBytes: n.agg.logicalBytes,
			FileCount:    n.agg.fileCount,
			DirCount:     n.agg.dirCount,
			SymlinkCount: n.agg.symlinkCount,
			DataStatus:   c.status,
			Accuracy:     c.accuracy,
			Truncated:    n.truncated,
			UpdatedAt:    now,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Depth != result[j].Depth {
			return result[i].Depth < result[j].Depth
		}
		return result[i].Path < result[j].Path
	})
	return result
}
