package cpauk

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestImportBoundaries(t *testing.T) {
	root := "."
	forbidden := []string{
		"/internal/api", "/internal/usagelimit", "/internal/managementasset",
		"/internal/tui", "/web/management-center",
	}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range file.Imports {
			value, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			for _, suffix := range forbidden {
				if strings.Contains(value, suffix) {
					t.Errorf("%s imports forbidden package %s", path, value)
				}
			}
			if strings.Contains(value, "/sdk/cliproxy/usage") && filepath.ToSlash(path) != "collector/adapter.go" && filepath.ToSlash(path) != "service.go" {
				t.Errorf("%s accepts usage outside the facade or collector adapter: %s", path, value)
			}
			if strings.HasPrefix(filepath.ToSlash(path), "collector/") && (value == "database/sql" || value == "encoding/json") {
				t.Errorf("request-side collector package %s imports %s", path, value)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	files, err := parser.ParseDir(token.NewFileSet(), "collector", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range files {
		for name, file := range pkg.Files {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			ast.Inspect(file, func(node ast.Node) bool {
				field, ok := node.(*ast.Field)
				if !ok {
					return true
				}
				selector, ok := field.Type.(*ast.SelectorExpr)
				if ok && selector.Sel.Name == "Record" && !strings.HasSuffix(name, "adapter.go") {
					t.Errorf("%s accepts a raw usage.Record outside the adapter", name)
				}
				return true
			})
		}
	}
}
