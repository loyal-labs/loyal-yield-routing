package webapp

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNoSessionSignerReachable(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".", "../../cmd/loyal-web").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, out)
	}
	for _, path := range strings.Fields(string(out)) {
		if strings.HasSuffix(path, "/internal/admin") || strings.Contains(path, "/internal/admin/") {
			t.Fatalf("internal/webapp must not import a JWT signer (internal/admin, reached as %s); move the shared code out of admin", path)
		}
	}
	sources, err := filepath.Glob("../jwt/*.go")
	if err != nil || len(sources) == 0 {
		t.Fatalf("internal/jwt sources: %v", err)
	}
	for _, source := range sources {
		file, err := parser.ParseFile(token.NewFileSet(), source, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.IsExported() && strings.HasPrefix(fn.Name.Name, "Sign") {
				t.Fatalf("internal/jwt must stay verify-only; move %s into internal/admin, unexported", fn.Name.Name)
			}
		}
	}
}
