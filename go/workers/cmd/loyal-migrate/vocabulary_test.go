package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const (
	yieldMigrations = "../../../../migrations/yield"
	internalDir     = "../../internal"
)

func TestGoVocabulariesMatchLatestMigrations(t *testing.T) {
	cases := []struct {
		name    string
		pkg     string
		goSet   func(*testing.T, string) []string
		symbol  string
		pattern string
	}{
		{"AttemptState", "autodeposit", typedConsts, "AttemptState",
			`balance_sweep_transaction_attempts_state_check\s+CHECK\s*\(\s*attempt_state\s+IN\s*\(([^)]*)\)`},
		{"ClaimStatus", "autodeposit", typedConsts, "ClaimStatus",
			`CREATE TYPE loyal_yield\.balance_sweep_lot_claim_status AS ENUM\s*\(([^)]*)\)`},
		{"LotStatus", "autodeposit", typedConsts, "LotStatus",
			`CREATE TYPE loyal_yield\.balance_sweep_surplus_lot_status AS ENUM\s*\(([^)]*)\)`},
		{"SubmissionState", "fleetexec", typedConsts, "SubmissionState",
			`signed_route_submissions_state_check\s+CHECK\s*\(\s*submission_state\s+IN\s*\(([^)]*)\)`},
		{"SubmissionTerminalStates", "fleet", stringSliceVar, "SubmissionTerminalStates",
			`signed_route_submissions_one_nonterminal_opportunity_idx[^;]*?WHERE\s+submission_state\s+NOT\s+IN\s*\(([^)]*)\)`},
		{"MovementLeg", "fleetexec", typedConsts, "MovementLeg",
			`signed_route_submissions_movement_leg_check\s+CHECK\s*\(\s*movement_leg\s+IN\s*\(([^)]*)\)`},
		{"LegPurpose", "fleetexec", typedConsts, "LegPurpose",
			`signed_route_submissions_movement_leg_check\s+CHECK\s*\(\s*movement_leg\s+IN\s*\([^)]*\)\s*AND\s+leg_purpose\s+IN\s*\(([^)]*)\)`},
		{"LookupKind", "fleetexec", typedConsts, "LookupKind",
			`lookup_table_operations_kind_check\s+CHECK\s*\(\s*operation_kind\s+IN\s*\(([^)]*)\)`},
		{"WakeupChannel", "autodeposit", stringConst, "WakeupChannel",
			`CREATE OR REPLACE FUNCTION loyal_yield\.notify_autodeposit_requested_slot\(\)[\s\S]*?pg_notify\(\s*('[^']+')`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.goSet(t, c.pkg+"/"+c.symbol)
			want := latestMigrationItems(t, c.pattern)
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("Go %s.%s = %q, latest migration = %q", c.pkg, c.symbol, got, want)
			}
		})
	}
}

func TestLeverageLadderMatchesMigrationAndKeepsItsGap(t *testing.T) {
	rungs := leverageRungs(t)
	var levels []float64
	for _, r := range rungs {
		levels = append(levels, r.level)
	}
	var want []float64
	for _, item := range latestMigrationItems(t, `backyard_lane_levels_level_check\s+CHECK\s*\(\s*level\s+IN\s*\(([^)]*)\)`) {
		v, err := strconv.ParseFloat(item, 64)
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, v)
	}
	slices.Sort(levels)
	slices.Sort(want)
	if !slices.Equal(levels, want) {
		t.Fatalf("leverageLadder levels = %v, latest migration = %v", levels, want)
	}
	for _, r := range rungs {
		if r.level > 1 && r.up-r.down < 0.01-1e-9 {
			t.Fatalf("level %v: down %v must sit at least 1 point under up %v", r.level, r.down, r.up)
		}
	}
}

type rung struct{ level, up, down float64 }

func latestMigrationItems(t *testing.T, pattern string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(yieldMigrations, "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("migrations: %v (%d files)", err, len(files))
	}
	slices.Sort(files)
	re := regexp.MustCompile(pattern)
	var list string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if m := re.FindSubmatch(b); m != nil {
			list = string(m[1])
		}
	}
	if list == "" {
		t.Fatalf("no migration matches %s", pattern)
	}
	var items []string
	for _, item := range strings.Split(list, ",") {
		items = append(items, strings.Trim(strings.TrimSpace(item), "'"))
	}
	return items
}

func parsePackage(t *testing.T, pkg string) []*ast.File {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(internalDir, pkg, "*.go"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("package %s: %v (%d files)", pkg, err, len(paths))
	}
	var files []*ast.File
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	return files
}

func unquote(t *testing.T, e ast.Expr) string {
	t.Helper()
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		t.Fatalf("expected a string literal, got %T", e)
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func typedConsts(t *testing.T, ref string) []string {
	pkg, typeName, _ := strings.Cut(ref, "/")
	var out []string
	for _, f := range parsePackage(t, pkg) {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, s := range gd.Specs {
				vs := s.(*ast.ValueSpec)
				if id, ok := vs.Type.(*ast.Ident); ok && id.Name == typeName {
					out = append(out, unquote(t, vs.Values[0]))
				}
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("no consts of type %s in %s", typeName, pkg)
	}
	return out
}

func varValue(t *testing.T, ref string, tok token.Token) ast.Expr {
	pkg, name, _ := strings.Cut(ref, "/")
	for _, f := range parsePackage(t, pkg) {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != tok {
				continue
			}
			for _, s := range gd.Specs {
				vs := s.(*ast.ValueSpec)
				if len(vs.Names) == 1 && vs.Names[0].Name == name {
					return vs.Values[0]
				}
			}
		}
	}
	t.Fatalf("%s not found", ref)
	return nil
}

func stringSliceVar(t *testing.T, ref string) []string {
	var out []string
	for _, e := range varValue(t, ref, token.VAR).(*ast.CompositeLit).Elts {
		out = append(out, unquote(t, e))
	}
	return out
}

func stringConst(t *testing.T, ref string) []string {
	return []string{unquote(t, varValue(t, ref, token.CONST))}
}

func leverageRungs(t *testing.T) []rung {
	var out []rung
	for _, e := range varValue(t, "backyard/leverageLadder", token.VAR).(*ast.CompositeLit).Elts {
		var r rung
		for _, kv := range e.(*ast.CompositeLit).Elts {
			kve := kv.(*ast.KeyValueExpr)
			v, err := strconv.ParseFloat(kve.Value.(*ast.BasicLit).Value, 64)
			if err != nil || math.IsNaN(v) {
				t.Fatal(err)
			}
			switch kve.Key.(*ast.Ident).Name {
			case "level":
				r.level = v
			case "up":
				r.up = v
			case "down":
				r.down = v
			}
		}
		out = append(out, r)
	}
	return out
}
