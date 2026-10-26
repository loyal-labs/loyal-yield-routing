package main

import (
	"errors"
	"fmt"
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
	"sync"
	"testing"
)

const (
	yieldMigrations = "../../../../migrations/yield"
	internalDir     = "../../internal"
)

func TestGoVocabulariesMatchLatestMigrations(t *testing.T) {
	cases := []struct {
		name   string
		pkg    string
		goSet  func(*testing.T, string) []string
		symbol string
		vocab  vocabulary
	}{
		{"AttemptState", "autodeposit", typedConsts, "AttemptState",
			vocabulary{checkConstraint, "balance_sweep_transaction_attempts_state_check", "balance_sweep_transaction_attempts", "attempt_state"}},
		{"ClaimStatus", "autodeposit", typedConsts, "ClaimStatus",
			vocabulary{enumType, "balance_sweep_lot_claim_status", "", ""}},
		{"LotStatus", "autodeposit", typedConsts, "LotStatus",
			vocabulary{enumType, "balance_sweep_surplus_lot_status", "", ""}},
		{"SubmissionState", "fleetexec", typedConsts, "SubmissionState",
			vocabulary{checkConstraint, "signed_route_submissions_state_check", "signed_route_submissions", "submission_state"}},
		{"SubmissionTerminalStates", "fleet", stringSliceVar, "SubmissionTerminalStates",
			vocabulary{partialIndex, "signed_route_submissions_one_nonterminal_opportunity_idx", "signed_route_submissions", "submission_state"}},
		{"MovementLeg", "fleetexec", typedConsts, "MovementLeg",
			vocabulary{checkConstraint, "signed_route_submissions_movement_leg_check", "signed_route_submissions", "movement_leg"}},
		{"LegPurpose", "fleetexec", typedConsts, "LegPurpose",
			vocabulary{checkConstraint, "signed_route_submissions_movement_leg_check", "signed_route_submissions", "leg_purpose"}},
		{"LookupKind", "fleetexec", typedConsts, "LookupKind",
			vocabulary{checkConstraint, "lookup_table_operations_kind_check", "lookup_table_operations", "operation_kind"}},
		{"WakeupChannel", "autodeposit", stringConst, "WakeupChannel",
			vocabulary{notifyFunction, "notify_autodeposit_requested_slot", "", ""}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.goSet(t, c.pkg+"/"+c.symbol)
			want, source := migrationVocabulary(t, c.vocab)
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("Go %s.%s = %q, %s = %q", c.pkg, c.symbol, got, source, want)
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
	wantItems, source := migrationVocabulary(t, vocabulary{checkConstraint, "backyard_lane_levels_level_check", "backyard_lane_levels", "level"})
	for _, item := range wantItems {
		v, err := strconv.ParseFloat(item, 64)
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, v)
	}
	slices.Sort(levels)
	slices.Sort(want)
	if !slices.Equal(levels, want) {
		t.Fatalf("leverageLadder levels = %v, %s = %v", levels, source, want)
	}
	for _, r := range rungs {
		if r.level > 1 && r.up-r.down < 0.01-1e-9 {
			t.Fatalf("level %v: down %v must sit at least 1 point under up %v", r.level, r.down, r.up)
		}
	}
}

type rung struct{ level, up, down float64 }

type vocabularyKind int

const (
	checkConstraint vocabularyKind = iota
	enumType
	partialIndex
	notifyFunction
)

type vocabulary struct {
	kind   vocabularyKind
	name   string
	table  string
	column string
}

type definition struct {
	present bool
	items   []string
	source  string
}

var (
	dollarTagRE       = regexp.MustCompile(`^\$(?:[A-Za-z_][A-Za-z0-9_]*)?\$`)
	doBlockRE         = regexp.MustCompile(`(?is)^DO\s+(?:LANGUAGE\s+\w+\s+)?(\$(?:[A-Za-z_][A-Za-z0-9_]*)?\$)`)
	beginRE           = regexp.MustCompile(`(?i)^BEGIN\s+`)
	enumStatementRE   = regexp.MustCompile(`(?i)\b(?:CREATE|ALTER|DROP)\s+TYPE\b|\bADD\s+VALUE\b|\bRENAME\s+VALUE\b|\bpg_enum\b`)
	dropConstraintRE  = regexp.MustCompile(`(?i)\bDROP\s+CONSTRAINT\s+(?:IF\s+EXISTS\s+)?$`)
	constraintRE      = regexp.MustCompile(`(?i)\bCONSTRAINT\s+$`)
	validateRE        = regexp.MustCompile(`(?i)\bVALIDATE\s+CONSTRAINT\s+$`)
	checkOpenRE       = regexp.MustCompile(`(?i)^\s*CHECK\s*\(`)
	dropIndexRE       = regexp.MustCompile(`(?i)\bDROP\s+INDEX\s+(?:CONCURRENTLY\s+)?(?:IF\s+EXISTS\s+)?(?:\w+\.)?$`)
	createIndexRE     = regexp.MustCompile(`(?i)\bCREATE\s+(?:UNIQUE\s+)?INDEX\s+(?:CONCURRENTLY\s+)?(?:IF\s+NOT\s+EXISTS\s+)?(?:\w+\.)?$`)
	whereRE           = regexp.MustCompile(`(?i)\bWHERE\b`)
	createFunctionRE  = regexp.MustCompile(`(?i)^CREATE\s+(?:OR\s+REPLACE\s+)?FUNCTION\s+(?:\w+\.)?$`)
	dropFunctionRE    = regexp.MustCompile(`(?i)^DROP\s+FUNCTION\s+(?:IF\s+EXISTS\s+)?(?:\w+\.)?$`)
	executeFunctionRE = regexp.MustCompile(`(?i)\bEXECUTE\s+(?:FUNCTION|PROCEDURE)\s+(?:\w+\.)?$`)
	pgNotifyRE        = regexp.MustCompile(`pg_notify\(\s*'([^']+)'`)
	listItemRE        = regexp.MustCompile(`'(?:[^']|'')*'(?:::[A-Za-z_][A-Za-z0-9_ ]*(?:\[\])?)?|-?\d+(?:\.\d+)?`)
)

type migrationFile struct {
	base  string
	stmts []string
	err   error
}

var loadMigrations = sync.OnceValues(func() ([]migrationFile, error) {
	files, err := filepath.Glob(filepath.Join(yieldMigrations, "*.sql"))
	if err != nil || len(files) == 0 {
		return nil, fmt.Errorf("migrations: %v (%d files)", err, len(files))
	}
	slices.Sort(files)
	out := make([]migrationFile, 0, len(files))
	for _, f := range files {
		base := filepath.Base(f)
		b, err := os.ReadFile(f)
		if err != nil {
			out = append(out, migrationFile{base: base, err: err})
			break
		}
		stmts, err := splitStatements(string(b))
		if err == nil {
			stmts, err = unwrapDoBlocks(stmts)
		}
		if err != nil {
			err = fmt.Errorf("%s: %w", base, err)
		}
		out = append(out, migrationFile{base: base, stmts: stmts, err: err})
		if err != nil {
			break
		}
	}
	return out, nil
})

func migrationVocabulary(t *testing.T, v vocabulary) ([]string, string) {
	t.Helper()
	files, err := loadMigrations()
	if err == nil {
		var items []string
		var source string
		items, source, err = readVocabulary(files, v)
		if err == nil {
			return items, source
		}
	}
	t.Fatal(err)
	return nil, ""
}

type vocabularyRE struct {
	mention, dropTable, dropColumn, listOpen, column, notIn, enumCreate, enumAlter *regexp.Regexp
}

func (v vocabulary) compile() vocabularyRE {
	table, column, name := regexp.QuoteMeta(v.table), regexp.QuoteMeta(v.column), regexp.QuoteMeta(v.name)
	enumName := `(?:\w+\.)?` + name
	return vocabularyRE{
		mention:    regexp.MustCompile(`(?i)\b` + name + `\b`),
		dropTable:  regexp.MustCompile(`(?i)^DROP\s+TABLE\b.*\b` + table + `\b`),
		dropColumn: regexp.MustCompile(`(?i)^ALTER\s+TABLE\s+(?:ONLY\s+)?(?:IF\s+EXISTS\s+)?(?:\w+\.)?` + table + `\b.*\bDROP\s+(?:COLUMN\s+)?(?:IF\s+EXISTS\s+)?` + column + `\b`),
		listOpen:   regexp.MustCompile(`(?i)\b` + column + `(?:\s+IN\s*(\()|\s*=\s*ANY\s*\(\s*ARRAY\s*(\[))`),
		column:     regexp.MustCompile(`(?i)\b` + column + `\b`),
		notIn:      regexp.MustCompile(`(?i)\b` + column + `\s+NOT\s+IN\s*\(`),
		enumCreate: regexp.MustCompile(`(?is)^CREATE\s+TYPE\s+` + enumName + `\s+AS\s+ENUM\s*\((.*)\)$`),
		enumAlter:  regexp.MustCompile(`(?is)^ALTER\s+TYPE\s+` + enumName + `\s+ADD\s+VALUE\s+(?:IF\s+NOT\s+EXISTS\s+)?('(?:[^']|'')*')(?:\s+(?:BEFORE|AFTER)\s+'(?:[^']|'')*')?$`),
	}
}

func readVocabulary(files []migrationFile, v vocabulary) ([]string, string, error) {
	re := v.compile()
	var def definition
	for _, f := range files {
		if f.err != nil {
			return nil, "", f.err
		}
		for _, s := range f.stmts {
			if v.removesBacking(s, re) {
				return nil, "", fmt.Errorf("%s: statement removes the table or column behind %s: %.200s", f.base, v.name, s)
			}
			locs := re.mention.FindAllStringIndex(s, -1)
			if len(locs) == 0 {
				continue
			}
			was, wasItems := def.present, slices.Clone(def.items)
			if err := v.apply(&def, s, locs, re); err != nil {
				return nil, "", fmt.Errorf("%s: cannot read the %s statement (%w): %.200s", f.base, v.name, err, s)
			}
			if def.present != was || !slices.Equal(def.items, wasItems) {
				def.source = f.base
			}
		}
	}
	if !def.present {
		return nil, "", fmt.Errorf("no migration leaves %s defined", v.name)
	}
	return def.items, def.source, nil
}

func (v vocabulary) removesBacking(stmt string, re vocabularyRE) bool {
	if v.table == "" {
		return false
	}
	return re.dropTable.MatchString(stmt) || re.dropColumn.MatchString(stmt)
}

func (v vocabulary) apply(def *definition, stmt string, locs [][]int, re vocabularyRE) error {
	if v.kind == enumType {
		return v.applyEnum(def, stmt, re)
	}
	for _, loc := range locs {
		before, after := stmt[:loc[0]], stmt[loc[1]:]
		var err error
		switch v.kind {
		case checkConstraint:
			err = v.applyCheck(def, before, after, re)
		case partialIndex:
			err = v.applyIndex(def, before, after, re)
		case notifyFunction:
			err = v.applyFunction(def, stmt, before)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (v vocabulary) applyEnum(def *definition, stmt string, re vocabularyRE) error {
	if !enumStatementRE.MatchString(stmt) {
		return nil
	}
	if m := re.enumCreate.FindStringSubmatch(stmt); m != nil {
		items, err := parseItems(m[1])
		if err != nil {
			return err
		}
		if def.present {
			if !sameSet(def.items, items) {
				return fmt.Errorf("type already exists with %q, so Postgres keeps it", def.items)
			}
			return nil
		}
		*def = definition{present: true, items: items}
		return nil
	}
	if m := re.enumAlter.FindStringSubmatch(stmt); m != nil && def.present {
		items, err := parseItems(m[1])
		if err == nil && !slices.Contains(def.items, items[0]) {
			def.items = append(def.items, items[0])
		}
		return err
	}
	return errors.New("not CREATE TYPE ... AS ENUM or ALTER TYPE ... ADD VALUE after the CREATE")
}

func (v vocabulary) applyCheck(def *definition, before, after string, re vocabularyRE) error {
	switch {
	case dropConstraintRE.MatchString(before):
		*def = definition{}
		return nil
	case validateRE.MatchString(before):
		return nil
	case constraintRE.MatchString(before) && checkOpenRE.MatchString(after):
		body, err := balanced(after[strings.Index(after, "("):], '(', ')')
		if err != nil {
			return err
		}
		hits := re.listOpen.FindAllStringSubmatchIndex(body, -1)
		if len(hits) != 1 {
			return fmt.Errorf("%d `%s IN (...)` or `%s = ANY(ARRAY[...])` forms, want 1", len(hits), v.column, v.column)
		}
		open, closing := hits[0][2], byte(')')
		if open < 0 {
			open, closing = hits[0][4], ']'
		}
		list, err := balanced(body[open:], body[open], closing)
		if err != nil {
			return err
		}
		items, err := parseItems(list)
		*def = definition{present: true, items: items}
		return err
	}
	return errors.New("neither DROP CONSTRAINT nor CONSTRAINT ... CHECK (")
}

func (v vocabulary) applyIndex(def *definition, before, after string, re vocabularyRE) error {
	switch {
	case dropIndexRE.MatchString(before):
		*def = definition{}
		return nil
	case createIndexRE.MatchString(before):
		where := whereRE.FindStringIndex(after)
		if where == nil {
			return errors.New("no WHERE clause")
		}
		predicate := after[where[1]:]
		if n := len(re.column.FindAllString(predicate, -1)); n != 1 || !re.notIn.MatchString(predicate) {
			return fmt.Errorf("WHERE is not a single `%s NOT IN (...)`", v.column)
		}
		loc := re.notIn.FindStringIndex(predicate)
		list, err := balanced(predicate[loc[1]-1:], '(', ')')
		if err != nil {
			return err
		}
		items, err := parseItems(list)
		if err != nil {
			return err
		}
		if def.present {
			if !slices.Equal(def.items, items) {
				return fmt.Errorf("index already exists with %q, so Postgres keeps it", def.items)
			}
			return nil
		}
		*def = definition{present: true, items: items}
		return nil
	}
	return errors.New("neither DROP INDEX nor CREATE INDEX")
}

func (v vocabulary) applyFunction(def *definition, stmt, before string) error {
	switch {
	case dropFunctionRE.MatchString(before):
		*def = definition{}
		return nil
	case executeFunctionRE.MatchString(before):
		return nil
	case createFunctionRE.MatchString(before):
		hits := pgNotifyRE.FindAllStringSubmatch(stmt, -1)
		if len(hits) != 1 {
			return fmt.Errorf("%d pg_notify('literal', ...) calls, want 1", len(hits))
		}
		*def = definition{present: true, items: []string{hits[0][1]}}
		return nil
	}
	return errors.New("neither CREATE FUNCTION, DROP FUNCTION nor EXECUTE FUNCTION")
}

func balanced(s string, open, closing byte) (string, error) {
	if s == "" || s[0] != open {
		return "", fmt.Errorf("expected %q", open)
	}
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\'':
			for i++; i < len(s) && s[i] != '\''; i++ {
			}
		case open:
			depth++
		case closing:
			depth--
			if depth == 0 {
				return s[1:i], nil
			}
		}
	}
	return "", fmt.Errorf("unbalanced %q", open)
}

func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

func parseItems(list string) ([]string, error) {
	rest := listItemRE.ReplaceAllString(list, "")
	if strings.Trim(rest, ", ") != "" {
		return nil, fmt.Errorf("list %q holds more than quoted or numeric literals", list)
	}
	var items []string
	for _, lit := range listItemRE.FindAllString(list, -1) {
		lit, _, _ = strings.Cut(lit, "::")
		items = append(items, strings.ReplaceAll(strings.Trim(lit, "'"), "''", "'"))
	}
	if len(items) == 0 {
		return nil, errors.New("empty list")
	}
	return items, nil
}

func splitStatements(sql string) ([]string, error) {
	var out []string
	var cur strings.Builder
	flush := func() {
		if s := strings.Join(strings.Fields(cur.String()), " "); s != "" {
			out = append(out, s)
		}
		cur.Reset()
	}
	for i := 0; i < len(sql); {
		rest := sql[i:]
		switch {
		case strings.HasPrefix(rest, "--"):
			n := strings.IndexByte(rest, '\n')
			if n < 0 {
				n = len(rest)
			}
			cur.WriteByte(' ')
			i += n
		case strings.HasPrefix(rest, "/*"):
			n := strings.Index(rest, "*/")
			if n < 0 {
				return nil, errors.New("unterminated block comment")
			}
			cur.WriteByte(' ')
			i += n + 2
		case rest[0] == '\'' || rest[0] == '"':
			n := 1
			for ; n < len(rest); n++ {
				if rest[n] != rest[0] {
					continue
				}
				if n+1 < len(rest) && rest[n+1] == rest[0] {
					n++
					continue
				}
				break
			}
			if n >= len(rest) {
				return nil, fmt.Errorf("unterminated %c quote", rest[0])
			}
			cur.WriteString(rest[:n+1])
			i += n + 1
		case dollarTagRE.MatchString(rest):
			tag := dollarTagRE.FindString(rest)
			n := strings.Index(rest[len(tag):], tag)
			if n < 0 {
				return nil, fmt.Errorf("unterminated %s quote", tag)
			}
			n += 2 * len(tag)
			cur.WriteString(rest[:n])
			i += n
		case rest[0] == ';':
			flush()
			i++
		default:
			cur.WriteByte(rest[0])
			i++
		}
	}
	flush()
	return out, nil
}

func unwrapDoBlocks(stmts []string) ([]string, error) {
	var out []string
	for _, s := range stmts {
		m := doBlockRE.FindStringSubmatch(s)
		if m == nil || strings.LastIndex(s, m[1]) < len(m[0]) {
			out = append(out, s)
			continue
		}
		inner, err := splitStatements(s[len(m[0]):strings.LastIndex(s, m[1])])
		if err == nil {
			inner, err = unwrapDoBlocks(inner)
		}
		if err != nil {
			return nil, err
		}
		for _, p := range inner {
			out = append(out, beginRE.ReplaceAllString(p, ""))
		}
	}
	return out, nil
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
			case "enterAt":
				r.up = v
			case "leaveBelow":
				r.down = v
			}
		}
		out = append(out, r)
	}
	return out
}
