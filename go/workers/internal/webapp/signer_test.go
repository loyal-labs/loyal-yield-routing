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

var signerReach = map[string]struct {
	owner string
	from  []string
}{
	"NewSigner":         {"internal/jwt", []string{"cmd/loyal-web/main.go"}},
	"Mint":              {"internal/jwt", []string{"internal/webapp/earn_session.go", "internal/webapp/earn_privy.go"}},
	"NewRealtimeIssuer": {"internal/jwt", []string{"cmd/loyal-web/main.go"}},
	"IssueRealtime":     {"internal/jwt", []string{"internal/webapp/state_realtime.go"}},
	"NewSponsor":        {"internal/earnprep", []string{"cmd/loyal-web/main.go"}},
	"Cosign":            {"internal/earnprep", []string{"internal/webapp/earn_sponsor.go"}},
}

func TestSigningStaysWithItsOps(t *testing.T) {
	module, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}|{{with .Module}}{{.Main}}{{end}}|{{.Dir}}|{{join .GoFiles \" \"}}|{{join .Imports \" \"}}", ".", "../../cmd/loyal-web").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, out)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.SplitN(line, "|", 5)
		if len(fields) != 5 {
			t.Fatalf("unexpected go list line %q", line)
		}
		path, main, dir, files, imports := fields[0], fields[1], fields[2], strings.Fields(fields[3]), strings.Fields(fields[4])
		if strings.HasSuffix(path, "/internal/admin") || strings.Contains(path, "/internal/admin/") {
			t.Fatalf("loyal-web must not import internal/admin (reached as %s): its signer is not one of loyal-web's two (jwt.Signer, earnprep.Sponsor); move the shared code out of admin", path)
		}
		if main != "true" {
			continue
		}
		pkg, err := filepath.Rel(module, dir)
		if err != nil {
			t.Fatal(err)
		}
		pkg = filepath.ToSlash(pkg)
		if pkg != "internal/jwt" && slices.Contains(imports, "crypto/hmac") {
			t.Fatalf("%s imports crypto/hmac: loyal-web signs sessions only through jwt.Signer, so keep HMAC inside internal/jwt", path)
		}
		for _, name := range files {
			file := pkg + "/" + name
			parsed, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, name), nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			ast.Inspect(parsed, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				rule, guarded := signerReach[sel.Sel.Name]
				if guarded && pkg != rule.owner && !slices.Contains(rule.from, file) {
					t.Errorf("%s calls %s: only %s may reach the %s signer (the session mint and Privy complete sign JWTs; the sponsored smart account create cosigns); move the call there", file, sel.Sel.Name, strings.Join(rule.from, ", "), rule.owner)
				}
				return true
			})
		}
	}
	sources, err := filepath.Glob("../jwt/*.go")
	if err != nil || len(sources) == 0 {
		t.Fatalf("internal/jwt sources: %v", err)
	}
	for _, source := range sources {
		if strings.HasSuffix(source, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), source, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !fn.Name.IsExported() || fn.Recv != nil || fn.Name.Name == "NewSigner" {
				continue
			}
			if strings.HasPrefix(fn.Name.Name, "Sign") || strings.HasPrefix(fn.Name.Name, "Mint") {
				t.Fatalf("internal/jwt exports %s: signing lives on jwt.Signer only (NewSigner, Signer.Mint), so the reach rule above can see every caller", fn.Name.Name)
			}
		}
	}
}
