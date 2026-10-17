/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestWatchHandlersNeverChangeTheObjectTheyAreGiven parses every controller. A handler passed to
// handler.EnqueueRequestsFromMapFunc receives the informer's shared cached object, so assigning to
// any field of it (or of a value type-asserted from it) changes what every other reader sees.
// connectionsForApp once set a LakeApp's namespace that way: every app identity on Lake/prod was
// hashed from the wrong namespace and the LakeApp controller could not find its own apps.
func TestWatchHandlersNeverChangeTheObjectTheyAreGiven(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	parsed := map[string]*ast.File{}
	handlers := map[string]bool{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		parsed[name] = file
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			if selector, ok := call.Fun.(*ast.SelectorExpr); !ok || selector.Sel.Name != "EnqueueRequestsFromMapFunc" {
				return true
			}
			switch fn := call.Args[0].(type) {
			case *ast.Ident:
				handlers[fn.Name] = true
			case *ast.SelectorExpr:
				handlers[fn.Sel.Name] = true
			}
			return true
		})
	}
	checked := 0
	for _, file := range parsed {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || !handlers[fn.Name.Name] {
				continue
			}
			checked++
			given := map[string]bool{}
			for _, field := range fn.Type.Params.List {
				if selector, ok := field.Type.(*ast.SelectorExpr); ok && selector.Sel.Name == "Object" {
					for _, ident := range field.Names {
						given[ident.Name] = true
					}
				}
			}
			// Values type-asserted from the given object are the same cached object.
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				assign, ok := node.(*ast.AssignStmt)
				if !ok || len(assign.Rhs) != 1 {
					return true
				}
				if assertion, ok := assign.Rhs[0].(*ast.TypeAssertExpr); ok {
					if ident, ok := assertion.X.(*ast.Ident); ok && given[ident.Name] {
						if left, ok := assign.Lhs[0].(*ast.Ident); ok {
							given[left.Name] = true
						}
					}
				}
				return true
			})
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				assign, ok := node.(*ast.AssignStmt)
				if !ok {
					return true
				}
				for _, left := range assign.Lhs {
					if root := assignedRoot(left); root != "" && given[root] {
						t.Errorf("%s: handler %s assigns to %s, the informer's shared cached object",
							fset.Position(assign.Pos()), fn.Name.Name, root)
					}
				}
				return true
			})
		}
	}
	if checked == 0 {
		t.Fatal("found no watch handlers; the test no longer recognises the code it guards")
	}
}

// assignedRoot names the variable whose field or element an assignment target writes, or "" when
// the target is a plain variable.
func assignedRoot(expr ast.Expr) string {
	inner := expr
	written := false
	for {
		switch e := inner.(type) {
		case *ast.SelectorExpr:
			inner, written = e.X, true
		case *ast.IndexExpr:
			inner, written = e.X, true
		case *ast.StarExpr:
			inner, written = e.X, true
		case *ast.ParenExpr:
			inner = e.X
		case *ast.Ident:
			if written {
				return e.Name
			}
			return ""
		default:
			return ""
		}
	}
}
