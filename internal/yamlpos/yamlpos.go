// Package yamlpos finds the source line of a value inside a YAML document.
//
// Extractors decode YAML into typed structures, which discards positions, but
// every node and edge in the graph carries evidence that names a file and a
// line. A second, cheap pass over the raw bytes recovers the line numbers
// without complicating the decoding path.
package yamlpos

import (
	"bytes"

	"go.yaml.in/yaml/v3"
)

// Locator indexes the documents in one YAML file.
type Locator struct {
	docs []*yaml.Node
}

// New parses content for position lookup. A parse error is not fatal to the
// caller: a Locator that found nothing returns line 0 for every query, and a
// missing line number is a far smaller loss than a failed extraction.
func New(content []byte) *Locator {
	l := &Locator{}
	dec := yaml.NewDecoder(bytes.NewReader(content))
	for {
		var doc yaml.Node
		if err := dec.Decode(&doc); err != nil {
			break
		}
		l.docs = append(l.docs, &doc)
	}
	return l
}

// Docs reports how many documents were parsed.
func (l *Locator) Docs() int { return len(l.docs) }

// Line returns the line of the value at the given mapping path, or 0 when the
// path is not present. Lookup is over every document in the file, first match
// winning, because a multi-document manifest is a single file to the caller.
func (l *Locator) Line(path ...string) int {
	if len(path) == 0 {
		return 0
	}
	for _, doc := range l.docs {
		if n := find(doc, path); n != nil {
			return n.Line
		}
	}
	return 0
}

// LineIn is Line scoped to a single document index, for multi-document files
// where the same key appears in several documents.
func (l *Locator) LineIn(doc int, path ...string) int {
	if len(path) == 0 || doc < 0 || doc >= len(l.docs) {
		return 0
	}
	if n := find(l.docs[doc], path); n != nil {
		return n.Line
	}
	return 0
}

// KeyLine is like Line but returns the position of the key rather than its
// value. For a block mapping the two are the same line; for a key whose value
// is on the next line they differ, and the key is what a reader looks for.
func (l *Locator) KeyLine(path ...string) int {
	if len(path) == 0 {
		return 0
	}
	parent, last := path[:len(path)-1], path[len(path)-1]
	for _, doc := range l.docs {
		n := find(doc, parent)
		if n == nil {
			continue
		}
		if k := key(n, last); k != nil {
			return k.Line
		}
	}
	return 0
}

func find(n *yaml.Node, path []string) *yaml.Node {
	cur := unwrap(n)
	for _, seg := range path {
		if cur == nil || cur.Kind != yaml.MappingNode {
			return nil
		}
		next := value(cur, seg)
		if next == nil {
			return nil
		}
		cur = unwrap(next)
	}
	return cur
}

// unwrap steps through document wrappers and alias indirection so that a
// merged or anchored mapping resolves like an inline one.
func unwrap(n *yaml.Node) *yaml.Node {
	for n != nil {
		switch {
		case n.Kind == yaml.DocumentNode && len(n.Content) > 0:
			n = n.Content[0]
		case n.Kind == yaml.AliasNode && n.Alias != nil:
			n = n.Alias
		default:
			return n
		}
	}
	return nil
}

func value(mapping *yaml.Node, k string) *yaml.Node {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == k {
			return mapping.Content[i+1]
		}
	}
	return nil
}

func key(mapping *yaml.Node, k string) *yaml.Node {
	if mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == k {
			return mapping.Content[i]
		}
	}
	return nil
}
