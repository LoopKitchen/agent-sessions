package skillusage

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The design 10.3 guards that fail the build rather than a row in
// production: skill arguments never reach a statement, and the value that
// hands a whole row to a copy (origin = derived, predicate U of the merge)
// is built by the derivation alone.

// parseDir parses every non-test Go file under dir.
func parseDir(t *testing.T, dir string) map[string]*ast.File {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	out := map[string]*ast.File{}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		out[filepath.Join(dir, name)] = f
	}
	return out
}

// TestArgumentsNeverReachAStatement walks this package and the store's
// skill files. An args-named value (an identifier or a field, whatever
// its case) may feed only a length or a presence: the operand of len,
// string or argsBytes, a comparison, or the definition of another
// args-named local. It may not be a call's argument, a literal's value, a
// return, or an operand of a statement, so the derived path keeps the
// length of tool_input.args and this route never has the text at all
// (its request struct has no such field; the decoder refuses the key).
func TestArgumentsNeverReachAStatement(t *testing.T) {
	files := parseDir(t, ".")
	for k, v := range parseDir(t, filepath.Join("..", "store")) {
		if strings.HasPrefix(filepath.Base(k), "skill") || filepath.Base(k) == "source_tokens.go" {
			files[k] = v
		}
	}
	isArgs := func(name string) bool { return strings.EqualFold(name, "args") }
	lengthLike := func(call *ast.CallExpr) bool {
		id, ok := call.Fun.(*ast.Ident)
		return ok && (id.Name == "len" || id.Name == "string" || id.Name == "argsBytes")
	}
	uses := 0
	for path, f := range files {
		var stack []ast.Node
		ast.Inspect(f, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
			var hit bool
			switch n := n.(type) {
			case *ast.SelectorExpr:
				hit = isArgs(n.Sel.Name)
			case *ast.Ident:
				hit = isArgs(n.Name)
			}
			if !hit || len(stack) < 2 {
				return true
			}
			uses++
			parent := stack[len(stack)-2]
			ok := false
			switch p := parent.(type) {
			case *ast.CallExpr:
				ok = lengthLike(p)
			case *ast.BinaryExpr:
				ok = true
			case *ast.AssignStmt:
				// args := <x>.Args, or the definition's left side.
				ok = true
				for _, r := range p.Rhs {
					if call, isCall := r.(*ast.CallExpr); isCall && !lengthLike(call) {
						ok = false
					}
				}
			case *ast.Field:
				// A struct field or a parameter named args: the store's
				// decode-only skillInput. This package may declare none.
				ok = strings.HasPrefix(path, "..")
			}
			if !ok {
				t.Errorf("%s: an args value is used outside a length or presence expression (%T)", path, parent)
			}
			// The field's own identifier is the selector's child; do not
			// count it again.
			if _, isSel := n.(*ast.SelectorExpr); isSel {
				stack = stack[:len(stack)-1]
				return false
			}
			return true
		})
	}
	if uses == 0 {
		t.Error("no args value found; the walk missed the derivation")
	}
	// The request struct has no such field at all, by name.
	for path, f := range parseDir(t, ".") {
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok || ts.Name.Name != "request" {
				return true
			}
			st := ts.Type.(*ast.StructType)
			for _, field := range st.Fields.List {
				for _, name := range field.Names {
					if isArgs(name.Name) {
						t.Errorf("%s: the request struct carries %s", path, name.Name)
					}
				}
				if field.Tag != nil {
					tag, _ := strconv.Unquote(field.Tag.Value)
					if strings.Contains(tag, `json:"args"`) || strings.Contains(tag, `json:"args,`) {
						t.Errorf("%s: a request field is tagged json:\"args\"", path)
					}
				}
			}
			return true
		})
	}
}

// TestOnlyTheDerivationBuildsADerivedRow walks every non-test file under
// server/: a SkillRow literal or an assignment that sets an Origin to
// OriginDerived (or the literal "derived") may sit only inside the
// derivation's row builder, since predicate U of the merge hands the
// whole row to any copy carrying that value. LS-1 factored the builders
// the design names (insertSkillInvocations, rebuildSessionSkillInvocations)
// into derivedRow, called from deriveSkillRows on both paths, so that one
// function is the whole allow set: a wider set would be slack a future
// builder could hide in.
func TestOnlyTheDerivationBuildsADerivedRow(t *testing.T) {
	allowed := map[string]bool{"derivedRow": true}
	root := filepath.Join("..")
	var files []string
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			files = append(files, path)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(files) < 20 {
		t.Fatalf("walked %d files under server/; expected the whole server tree", len(files))
	}
	fset := token.NewFileSet()
	isDerived := func(e ast.Expr) bool {
		switch e := e.(type) {
		case *ast.Ident:
			return e.Name == "OriginDerived"
		case *ast.SelectorExpr:
			return e.Sel.Name == "OriginDerived"
		case *ast.BasicLit:
			s, _ := strconv.Unquote(e.Value)
			return e.Kind == token.STRING && s == "derived"
		}
		return false
	}
	isSkillRow := func(e ast.Expr) bool {
		switch e := e.(type) {
		case *ast.Ident:
			return e.Name == "SkillRow"
		case *ast.SelectorExpr:
			return e.Sel.Name == "SkillRow"
		}
		return false
	}
	isOrigin := func(e ast.Expr) bool {
		switch e := e.(type) {
		case *ast.Ident:
			return e.Name == "Origin"
		case *ast.SelectorExpr:
			return e.Sel.Name == "Origin"
		}
		return false
	}
	sites := 0
	for _, path := range files {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				hit := false
				switch n := n.(type) {
				case *ast.CompositeLit:
					if !isSkillRow(n.Type) {
						return true
					}
					for _, e := range n.Elts {
						if kv, ok := e.(*ast.KeyValueExpr); ok && isOrigin(kv.Key) && isDerived(kv.Value) {
							hit = true
						}
					}
				case *ast.AssignStmt:
					for i, lhs := range n.Lhs {
						if i < len(n.Rhs) && isOrigin(lhs) && isDerived(n.Rhs[i]) {
							hit = true
						}
					}
				}
				if hit {
					sites++
					if !allowed[fn.Name.Name] {
						t.Errorf("%s: %s builds a row with origin = derived; only the derivation may", path, fn.Name.Name)
					}
				}
				return true
			})
		}
	}
	if sites == 0 {
		t.Error("no site builds origin = derived; the walk missed the derivation")
	}
}

// TestTheRequestStructHasNoTrustOrActorColumn: trust and the token
// columns are the credential's, so the wire struct cannot even name them.
// actor_email is declared to be refused by name and is never read.
func TestTheRequestStructHasNoTrustOrActorColumn(t *testing.T) {
	for path, f := range parseDir(t, ".") {
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok || ts.Name.Name != "request" {
				return true
			}
			for _, field := range ts.Type.(*ast.StructType).Fields.List {
				for _, name := range field.Names {
					switch strings.ToLower(name.Name) {
					case "trust", "sourcetokenid", "deviceid", "actorknown", "tokenplatform", "tokenenvironment":
						t.Errorf("%s: the request struct names %s, a credential column", path, name.Name)
					}
				}
			}
			return true
		})
	}
}
