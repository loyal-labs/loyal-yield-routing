package webapp

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const jwtPackage = "/internal/jwt"

func TestNoSessionSignerReachable(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}|{{with .Module}}{{.Main}}{{end}}|{{join .Imports \" \"}}", ".", "../../cmd/loyal-web").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, out)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.SplitN(line, "|", 3)
		if len(fields) != 3 {
			t.Fatalf("unexpected go list line %q", line)
		}
		path, main, imports := fields[0], fields[1], strings.Fields(fields[2])
		if strings.HasSuffix(path, "/internal/admin") || strings.Contains(path, "/internal/admin/") {
			t.Fatalf("internal/webapp must not import a JWT signer (internal/admin, reached as %s); move the shared code out of admin", path)
		}
		if main != "true" || strings.HasSuffix(path, jwtPackage) {
			continue
		}
		if slices.Contains(imports, "crypto/hmac") {
			t.Fatalf("%s imports crypto/hmac; loyal-web only verifies sessions, so keep HMAC inside internal/jwt (jwt.NewKeys, Keys.Verify) and never sign", path)
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
