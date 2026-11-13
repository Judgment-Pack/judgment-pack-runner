package runner

// No code outside the sqlite kind's files holds the kind's database or makes
// a statement (docs/design/store-interface.md, section 9.5): the rule is
// read from the module's source, not from a list of files. Outside the files
// sqliteKindFiles names, no file of the module imports database/sql or a
// sqlite package, or holds statement text in a string; and no file of this
// package selects a member the kind's files declare with a database/sql type,
// or names a function, variable or type they declare over one.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// sqliteKindFiles are the sqlite kind's files, from the module's root.
const sqliteKindFiles = "internal/runner/store_sqlite*.go"

// sqlStatement is statement text: a statement's keywords, written as this
// module writes them, or a function only a statement calls.
var sqlStatement = regexp.MustCompile(`\b(SELECT\b[\s\S]*\bFROM|INSERT (OR [A-Z]+ )?INTO|UPDATE [a-z_]+ SET|DELETE FROM|(CREATE|DROP|ALTER) (TABLE|(UNIQUE )?INDEX|TRIGGER|VIEW)|PRAGMA [a-z_]+|BEGIN (IMMEDIATE|EXCLUSIVE|DEFERRED)\b|json_(extract|set|remove|insert|replace|each)\(|count\(\*\))`)

type lintedFile struct {
	path string // from the module's root, with slashes
	file *ast.File
	kind bool // one of sqliteKindFiles
}

// moduleSources parses every Go file of the module outside its tests.
func moduleSources(t *testing.T, fset *token.FileSet) []lintedFile {
	t.Helper()
	root := filepath.Join("..", "..")
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatal("the module's root is not two directories up:", err)
	}
	var files []lintedFile
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if p != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata" || name == "vendor" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		kind, err := path.Match(sqliteKindFiles, rel)
		files = append(files, lintedFile{path: rel, file: f, kind: kind})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// importNames are the names f's imports are known by, and the name f knows
// database/sql by, if it imports it.
func importNames(f *ast.File) (names map[string]bool, sqlName string) {
	names = map[string]bool{}
	for _, spec := range f.Imports {
		p, _ := strconv.Unquote(spec.Path.Value)
		name := path.Base(p)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		names[name] = true
		if p == "database/sql" {
			sqlName = name
		}
	}
	return names, sqlName
}

// mentions says whether expr names a database/sql type, through sqlName, or
// one of over.
func mentions(expr ast.Node, sqlName string, over map[string]bool) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			if id, ok := n.X.(*ast.Ident); ok && sqlName != "" && id.Name == sqlName {
				found = true
			}
			return false
		case *ast.Ident:
			if over[n.Name] {
				found = true
			}
		}
		return !found
	})
	return found
}

func TestNoStatementOutsideTheSQLiteKind(t *testing.T) {
	fset := token.NewFileSet()
	files := moduleSources(t, fset)
	at := func(n ast.Node) string { return fset.Position(n.Pos()).String() }

	// What the kind's files declare over database/sql: the types (an
	// interface or a function over it, not a struct that keeps it), the
	// members of a type, and the functions and variables.
	over, members, named := map[string]bool{}, map[string]bool{}, map[string]bool{}
	kindFiles, statements := 0, 0
	for changed := true; changed; {
		changed = false
		for _, lf := range files {
			if !lf.kind {
				continue
			}
			_, sqlName := importNames(lf.file)
			for _, decl := range lf.file.Decls {
				g, ok := decl.(*ast.GenDecl)
				if !ok {
					continue
				}
				for _, spec := range g.Specs {
					if ts, ok := spec.(*ast.TypeSpec); ok && !over[ts.Name.Name] {
						if _, keeps := ts.Type.(*ast.StructType); !keeps && mentions(ts.Type, sqlName, over) {
							over[ts.Name.Name], changed = true, true
						}
					}
				}
			}
		}
	}
	for _, lf := range files {
		if !lf.kind {
			continue
		}
		kindFiles++
		_, sqlName := importNames(lf.file)
		for _, decl := range lf.file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if mentions(d.Type, sqlName, over) {
					if d.Recv != nil {
						members[d.Name.Name] = true
					} else {
						named[d.Name.Name] = true
					}
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok && vs.Type != nil && mentions(vs.Type, sqlName, over) {
						for _, name := range vs.Names {
							named[name.Name] = true
						}
					}
				}
			}
		}
		ast.Inspect(lf.file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.StructType:
				for _, field := range n.Fields.List {
					if mentions(field.Type, sqlName, over) {
						for _, name := range field.Names {
							members[name.Name] = true
						}
					}
				}
			case *ast.BasicLit:
				if text, err := strconv.Unquote(n.Value); n.Kind == token.STRING && err == nil && sqlStatement.MatchString(text) {
					statements++
				}
			}
			return true
		})
	}
	for name := range over {
		named[name] = true
	}
	// A member name the package also declares over other types is not one a
	// selector can be told apart by, and is left to the rules above.
	for _, lf := range files {
		if !strings.HasPrefix(lf.path, "internal/runner/") || strings.Contains(strings.TrimPrefix(lf.path, "internal/runner/"), "/") {
			continue
		}
		_, sqlName := importNames(lf.file)
		ast.Inspect(lf.file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.StructType:
				for _, field := range n.Fields.List {
					if !mentions(field.Type, sqlName, over) {
						for _, name := range field.Names {
							if members[name.Name] {
								t.Logf("%s: the member %s is also declared over another type; selectors of it are not checked", at(name), name.Name)
								delete(members, name.Name)
							}
						}
					}
				}
			case *ast.FuncDecl:
				if n.Recv != nil && members[n.Name.Name] && !mentions(n.Type, sqlName, over) {
					t.Logf("%s: the method %s is also declared over another type; selectors of it are not checked", at(n.Name), n.Name.Name)
					delete(members, n.Name.Name)
				}
			}
			return true
		})
	}
	if kindFiles == 0 || statements == 0 || len(members) == 0 {
		t.Fatalf("the rule reads nothing: %d kind files, %d statements, %d members over database/sql", kindFiles, statements, len(members))
	}

	for _, lf := range files {
		if lf.kind {
			continue
		}
		imports, _ := importNames(lf.file)
		for _, spec := range lf.file.Imports {
			p, _ := strconv.Unquote(spec.Path.Value)
			if p == "database/sql" || strings.HasPrefix(p, "database/sql/") || strings.Contains(p, "sqlite") {
				t.Errorf("%s: imports %s outside the sqlite kind's files (%s)", at(spec), p, sqliteKindFiles)
			}
		}
		ownPackage := path.Dir(lf.path) == "internal/runner"
		selected := map[*ast.Ident]bool{}
		ast.Inspect(lf.file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.BasicLit:
				if text, err := strconv.Unquote(n.Value); n.Kind == token.STRING && err == nil && sqlStatement.MatchString(text) {
					t.Errorf("%s: statement text outside the sqlite kind's files: %.60q", at(n), text)
				}
			case *ast.SelectorExpr:
				selected[n.Sel] = true
				if id, ok := n.X.(*ast.Ident); ok && imports[id.Name] {
					return false
				}
				if ownPackage && members[n.Sel.Name] {
					t.Errorf("%s: selects %s, which the sqlite kind declares over database/sql", at(n.Sel), n.Sel.Name)
				}
			case *ast.Ident:
				if ownPackage && !selected[n] && named[n.Name] {
					t.Errorf("%s: names %s, which the sqlite kind declares over database/sql", at(n), n.Name)
				}
			}
			return true
		})
	}
}
