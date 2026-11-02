package config

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

var yamlErrorLineRef = regexp.MustCompile(`\bline (\d+): `)

var yamlUnknownField = regexp.MustCompile(`^line (\d+): field (.+) not found in type (\S+)$`)

// yamlCounterpartOnLine finds the first node of rendered on line, where rendered
// is yaml.Marshal(tree) parsed back, and returns the same node in tree with its
// dotted path. Scalars (and so mapping keys) win over the collections that start
// on the same line. A non-empty key only matches mapping keys with that value.
func yamlCounterpartOnLine(rendered, tree *yaml.Node, line int, key string) (*yaml.Node, string) {
	for _, scalarsOnly := range []bool{true, false} {
		if node, path, ok := findYAMLCounterpart(rendered, tree, line, key, scalarsOnly, "", false); ok {
			return node, path
		}
	}
	return nil, ""
}

func findYAMLCounterpart(rendered, tree *yaml.Node, line int, key string, scalarsOnly bool, path string, isKey bool) (*yaml.Node, string, bool) {
	if rendered == nil || tree == nil || rendered.Kind != tree.Kind || len(rendered.Content) != len(tree.Content) {
		return nil, "", false
	}
	if rendered.Line == line && (!scalarsOnly || rendered.Kind == yaml.ScalarNode) && (key == "" || isKey && rendered.Value == key) {
		return tree, path, true
	}
	for i := 0; i < len(rendered.Content); i++ {
		childPath, childIsKey := path, false
		switch rendered.Kind {
		case yaml.MappingNode:
			keyIndex := i - i%2
			childPath, childIsKey = joinYAMLPath(path, tree.Content[keyIndex].Value), i%2 == 0
		case yaml.SequenceNode:
			childPath = path + "[" + strconv.Itoa(i) + "]"
		}
		if node, found, ok := findYAMLCounterpart(rendered.Content[i], tree.Content[i], line, key, scalarsOnly, childPath, childIsKey); ok {
			return node, found, true
		}
	}
	return nil, "", false
}

func joinYAMLPath(parent, key string) string {
	if parent == "" {
		return key
	}
	return parent + "." + key
}

// rewriteYAMLErrorLines replaces each "line N: " in msg, which points into
// rendered = yaml.Marshal(tree), with format applied to the same node in tree.
// References that cannot be resolved stay unchanged.
func rewriteYAMLErrorLines(msg string, rendered []byte, tree *yaml.Node, format func(node *yaml.Node, path string) string) string {
	var doc yaml.Node
	if yaml.Unmarshal(rendered, &doc) != nil || len(doc.Content) == 0 {
		return msg
	}
	if tree != nil && tree.Kind == yaml.DocumentNode && len(tree.Content) > 0 {
		tree = tree.Content[0]
	}
	return yamlErrorLineRef.ReplaceAllStringFunc(msg, func(ref string) string {
		line, _ := strconv.Atoi(yamlErrorLineRef.FindStringSubmatch(ref)[1])
		node, path := yamlCounterpartOnLine(doc.Content[0], tree, line, "")
		if node == nil {
			return ref
		}
		return format(node, path)
	})
}

// describeV8DecodeError restates a strict legacy decode error in terms of the
// v8 document the caller passed to ValidateV8Config. flat carries that
// document's node positions; encoded is yaml.Marshal(flat). Unknown fields are
// reported at the unknown key itself rather than at the v8 section that was
// left behind by flattening, and without internal Go type names.
func describeV8DecodeError(err error, encoded []byte, flat *yaml.Node) error {
	var typeErr *yaml.TypeError
	if !errors.As(err, &typeErr) {
		return err
	}
	var rendered yaml.Node
	if yaml.Unmarshal(encoded, &rendered) != nil || len(rendered.Content) == 0 {
		return err
	}
	atLine := func(node *yaml.Node) string {
		if node.Line == 0 {
			return ""
		}
		return "line " + strconv.Itoa(node.Line) + ": "
	}
	var lines []string
	for _, entry := range typeErr.Errors {
		match := yamlUnknownField.FindStringSubmatch(entry)
		if match == nil {
			lines = append(lines, rewriteYAMLErrorLines(entry, encoded, flat, func(node *yaml.Node, _ string) string { return atLine(node) }))
			continue
		}
		line, _ := strconv.Atoi(match[1])
		key, path := yamlCounterpartOnLine(rendered.Content[0], flat, line, match[2])
		if key == nil {
			lines = append(lines, entry)
			continue
		}
		if match[3] != "config.legacyConfig" {
			lines = append(lines, atLine(key)+"field "+match[2]+" not found")
			continue
		}
		// Flattening moves every known v8 field out of its section, so what is
		// left under a v8 section path consists of unknown fields only.
		for _, unknown := range unknownV8Keys(yamlPath(flat, path), path) {
			lines = append(lines, atLine(unknown)+"field "+unknown.Value+" not found")
		}
	}
	if len(lines) == 0 {
		return err
	}
	return fmt.Errorf("yaml: unmarshal errors:\n  %s", strings.Join(lines, "\n  "))
}

// unknownV8Keys descends through v8 section mappings and returns the keys that
// are not sections themselves.
func unknownV8Keys(node *yaml.Node, path string) []*yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	var keys []*yaml.Node
	for i := 0; i+1 < len(node.Content); i += 2 {
		child := joinYAMLPath(path, node.Content[i].Value)
		if node.Content[i+1].Kind == yaml.MappingNode && isV8Section(child) {
			keys = append(keys, unknownV8Keys(node.Content[i+1], child)...)
			continue
		}
		keys = append(keys, node.Content[i])
	}
	return keys
}

// isV8Section reports whether path is a proper prefix of a known v8 field path.
// Aliases and shared struct paths spell v8 locations on both sides.
func isV8Section(path string) bool {
	prefix := path + "."
	for _, candidate := range v8Paths {
		if strings.HasPrefix(candidate.current, prefix) {
			return true
		}
	}
	for _, candidate := range append(append([]configPath(nil), v8Aliases...), v8SharedStructPaths...) {
		if strings.HasPrefix(candidate.current, prefix) || strings.HasPrefix(candidate.old, prefix) {
			return true
		}
	}
	return false
}
