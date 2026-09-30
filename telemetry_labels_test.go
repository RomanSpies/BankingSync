package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

var (
	instrumentConstructors = map[string]bool{
		"Int64Counter": true, "Float64Counter": true,
		"Int64UpDownCounter": true, "Float64UpDownCounter": true,
		"Int64Histogram": true, "Float64Histogram": true,
		"Int64ObservableGauge": true, "Float64ObservableGauge": true,
	}
	attributeConstructors = map[string]bool{
		"String": true, "Int": true, "Int64": true, "Bool": true, "Float64": true,
		"StringSlice": true, "IntSlice": true, "Int64Slice": true, "BoolSlice": true, "Float64Slice": true,
	}
	docMetricRowRE = regexp.MustCompile("^\\| `(bankingsync_\\w+)` \\| \\w+ \\| ([^|]*)\\|")
	docLabelRE     = regexp.MustCompile("`(\\w+)`")
)

type telemetryPackage struct {
	files []*ast.File
	funcs map[string]*ast.FuncDecl
	vars  map[string]ast.Expr
}

func parseTelemetryPackages(t *testing.T) map[string]*telemetryPackage {
	t.Helper()
	fset := token.NewFileSet()
	pkgs := map[string]*telemetryPackage{}
	for path, src := range sourceFiles(t) {
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		dir := filepath.Dir(path)
		p := pkgs[dir]
		if p == nil {
			p = &telemetryPackage{funcs: map[string]*ast.FuncDecl{}, vars: map[string]ast.Expr{}}
			pkgs[dir] = p
		}
		p.files = append(p.files, f)
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				p.funcs[d.Name.Name] = d
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok {
						for i, name := range vs.Names {
							if i < len(vs.Values) {
								p.vars[name.Name] = vs.Values[i]
							}
						}
					}
				}
			}
		}
	}
	return pkgs
}

func selectorName(e ast.Expr) (recv ast.Expr, name string, ok bool) {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return nil, "", false
	}
	return sel.X, sel.Sel.Name, true
}

func instrumentName(call *ast.CallExpr) (string, bool) {
	_, ctor, ok := selectorName(call.Fun)
	if !ok || !instrumentConstructors[ctor] || len(call.Args) == 0 {
		return "", false
	}
	lit, ok := call.Args[0].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	name, err := strconv.Unquote(lit.Value)
	if err != nil || !strings.HasPrefix(name, "bankingsync_") {
		return "", false
	}
	return name, true
}

func enclosingFunc(f *ast.File, n ast.Node) *ast.FuncDecl {
	for _, decl := range f.Decls {
		if fd, ok := decl.(*ast.FuncDecl); ok && fd.Pos() <= n.Pos() && n.End() <= fd.End() {
			return fd
		}
	}
	return nil
}

func localDefinition(scope ast.Node, name string) ast.Expr {
	if scope == nil {
		return nil
	}
	var found ast.Expr
	ast.Inspect(scope, func(n ast.Node) bool {
		if as, ok := n.(*ast.AssignStmt); ok && found == nil {
			for i, lhs := range as.Lhs {
				if id, ok := lhs.(*ast.Ident); ok && id.Name == name && i < len(as.Rhs) {
					found = as.Rhs[i]
				}
			}
		}
		return found == nil
	})
	return found
}

type labelCollector struct {
	pkg     *telemetryPackage
	labels  map[string]bool
	unknown []string
	seen    map[string]bool
}

func (c *labelCollector) collect(scope ast.Node, e ast.Node) {
	ast.Inspect(e, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			recv, fn, ok := selectorName(x.Fun)
			if id, isIdent := recv.(*ast.Ident); ok && isIdent && id.Name == "attribute" && attributeConstructors[fn] {
				if lit, ok := x.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					key, _ := strconv.Unquote(lit.Value)
					c.labels[key] = true
				} else {
					c.unknown = append(c.unknown, "a key that is not a literal")
				}
				return false
			}
		case *ast.Ident:
			if c.seen[x.Name] {
				return false
			}
			if def := localDefinition(scope, x.Name); def != nil {
				c.seen[x.Name] = true
				c.collect(scope, def)
			} else if def, ok := c.pkg.vars[x.Name]; ok {
				c.seen[x.Name] = true
				c.collect(scope, def)
			}
		}
		return true
	})
}

func (c *labelCollector) recordingSite(scope ast.Node, call *ast.CallExpr) {
	for _, arg := range call.Args[1:] {
		c.collect(scope, arg)
	}
}

func (c *labelCollector) observations(scope ast.Node, root ast.Node, depth int) {
	ast.Inspect(root, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if _, fn, ok := selectorName(call.Fun); ok {
			if fn == "Observe" {
				c.recordingSite(scope, call)
				return false
			}
			if fd, local := c.pkg.funcs[fn]; local && depth < 3 && !c.seen["func:"+fn] {
				c.seen["func:"+fn] = true
				c.observations(fd, fd.Body, depth+1)
			}
		}
		return true
	})
}

func instrumentFields(f *ast.File, variable string) []string {
	var fields []string
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.KeyValueExpr:
			key, kOK := x.Key.(*ast.Ident)
			val, vOK := x.Value.(*ast.Ident)
			if kOK && vOK && val.Name == variable {
				fields = append(fields, key.Name)
			}
		case *ast.AssignStmt:
			for i, rhs := range x.Rhs {
				if val, ok := rhs.(*ast.Ident); ok && val.Name == variable && i < len(x.Lhs) {
					if _, field, ok := selectorName(x.Lhs[i]); ok {
						fields = append(fields, field)
					}
				}
			}
		}
		return true
	})
	return fields
}

func codeLabels(t *testing.T) (labels map[string]map[string]bool, problems []string) {
	t.Helper()
	labels = map[string]map[string]bool{}
	for dir, pkg := range parseTelemetryPackages(t) {
		for _, f := range pkg.files {
			ast.Inspect(f, func(n ast.Node) bool {
				as, ok := n.(*ast.AssignStmt)
				if !ok || len(as.Rhs) != 1 {
					return true
				}
				call, ok := as.Rhs[0].(*ast.CallExpr)
				if !ok {
					return true
				}
				name, ok := instrumentName(call)
				if !ok {
					return true
				}
				c := &labelCollector{pkg: pkg, labels: map[string]bool{}, seen: map[string]bool{}}
				sites := 0
				if _, ctor, _ := selectorName(call.Fun); strings.Contains(ctor, "Observable") {
					c.observations(enclosingFunc(f, call), call, 0)
					sites = 1
				} else if id, ok := as.Lhs[0].(*ast.Ident); ok {
					for _, field := range instrumentFields(f, id.Name) {
						for _, other := range pkg.files {
							ast.Inspect(other, func(n ast.Node) bool {
								rec, ok := n.(*ast.CallExpr)
								if !ok {
									return true
								}
								inst, method, ok := selectorName(rec.Fun)
								if !ok || (method != "Add" && method != "Record") {
									return true
								}
								if _, used, ok := selectorName(inst); ok && used == field {
									sites++
									c.recordingSite(enclosingFunc(other, rec), rec)
								}
								return true
							})
						}
					}
				}
				if sites == 0 {
					problems = append(problems, name+" ("+dir+"): no place that records it was found")
				}
				for _, u := range c.unknown {
					problems = append(problems, name+" ("+dir+"): "+u)
				}
				labels[name] = c.labels
				return true
			})
		}
	}
	return labels, problems
}

func docLabels(t *testing.T) map[string]map[string]bool {
	t.Helper()
	out := map[string]map[string]bool{}
	for line := range strings.SplitSeq(readTelemetryDoc(t), "\n") {
		m := docMetricRowRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		labels := map[string]bool{}
		for _, l := range docLabelRE.FindAllStringSubmatch(m[2], -1) {
			labels[l[1]] = true
		}
		out[m[1]] = labels
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	return slices.Sorted(maps.Keys(m))
}

func TestTelemetryDoc_listsTheLabelsEachInstrumentIsRecordedWith(t *testing.T) {
	code, problems := codeLabels(t)
	if len(code) < 40 {
		t.Fatalf("only %d instruments found; the walk is not reaching their declarations", len(code))
	}
	for _, p := range problems {
		t.Errorf("cannot establish the labels of %s", p)
	}

	doc := docLabels(t)
	for _, name := range slices.Sorted(maps.Keys(code)) {
		documented, ok := doc[name]
		if !ok {
			t.Errorf("%s has no row in the metric tables of %s", name, telemetryDoc)
			continue
		}
		if !maps.Equal(code[name], documented) {
			t.Errorf("%s is recorded with labels %v but documented with %v; a query written "+
				"from the document would select nothing or aggregate the wrong series",
				name, sortedKeys(code[name]), sortedKeys(documented))
		}
	}
}
