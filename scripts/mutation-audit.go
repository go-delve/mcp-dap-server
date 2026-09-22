//go:build ignore

// mutation-audit runs sequential, per-test production mutations using Go build
// overlays. Neither production files nor tests are rewritten by this program.
// Run with go run scripts/mutation-audit.go -help.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

type mutation struct {
	File, Function, Operator, Before, After, Result, Log string
	Line, Start, End                                     int
}

type testResult struct {
	Test, File, Status, Reason, BaselineLog string
	Line, CoveredStatements, Candidates     int
	Assertions                              []int
	Mutations                               []mutation
}

type sourceFile struct {
	Name string
	Data []byte
	AST  *ast.File
}

type audit struct {
	root, output, helper, fingerprint string
	files                             []*sourceFile
	fset                              *token.FileSet
	testSource                        map[string]*ast.FuncDecl
	testFiles                         map[string]string
	max                               int
	run                               string
	results                           []testResult
}

var subjects = map[string]string{
	"TestBasic": "evaluateExpression", "TestRestart": "restartDebugger",
	"TestRestartUsesSavedLaunchArguments": "RestartRequest", "TestReplFrameSelection": "replFrameSelection",
	"TestContext": "getFullContext", "TestVariables": "writeVariable",
	"TestStep": "step", "TestStepIn": "step", "TestStepOut": "step",
	"TestCoreDump": "debug", "TestToolListChangesWithCapabilities": "registerSessionTools",
	"TestDelveRejectsNonGoBinaryWithDebuggerGuidance": "debug",
	"TestClearBreakpoints":                            "clearBreakpoints", "TestInfoBreakpoints": "info",
	"TestLineBreakpointTracking": "addLineBreakpoint", "TestClearSpecificLineBreakpoint": "removeLineBreakpoint",
	"TestMultipleFilesLineBreakpoints": "addLineBreakpoint", "TestClearAllBreakpointsAcrossFiles": "clearAllLineBreakpoints",
	"TestDuplicateLineBreakpoint": "addLineBreakpoint", "TestRunToCursorCleansUpBreakpoint": "continueExecution",
	"TestContinueWithEmptyTo": "continueExecution", "TestContinueWithPartialTo": "continueExecution",
	"TestInfo": "info", "TestDisassemble": "disassembleCode", "TestSetVariable": "setVariable",
	"TestPause": "pauseExecution", "TestErrorBeforeDebuggerStarted": "registerTools",
	"TestTerminationMessage": "waitForStopOrTermination", "TestStopOnEntry": "debug",
	"TestFlexIntUnmarshal": "UnmarshalJSON", "TestFlexIntInStruct": "UnmarshalJSON",
	"TestDebugBreakpointAcceptsStringLine": "UnmarshalJSON", "TestReadMessageTimeoutClosesConnection": "ReadMessage",
}

func main() {
	a := &audit{fset: token.NewFileSet(), testSource: map[string]*ast.FuncDecl{}, testFiles: map[string]string{}}
	home, _ := os.UserHomeDir()
	defaultHelper := filepath.Join("scripts", "goaudit-ci.sh")
	if _, err := os.Stat(defaultHelper); err != nil {
		defaultHelper = filepath.Join(home, ".agents/skills/go-test-mutation-audit/scripts/goaudit.sh")
	}
	flag.StringVar(&a.helper, "helper", defaultHelper, "mutation audit helper path")
	flag.StringVar(&a.output, "output", "", "required external directory for logs, overlays, resumable results")
	flag.IntVar(&a.max, "max", 5, "mutations per test; 0 runs every generated covered candidate")
	flag.StringVar(&a.run, "run", ".*", "regular expression selecting top-level tests")
	flag.Parse()
	var err error
	a.root, err = os.Getwd()
	must(err)
	if a.output == "" || a.max < 0 {
		fatal("provide -output outside the repository and a nonnegative -max")
	}
	a.output, err = filepath.Abs(a.output)
	must(err)
	if rel, err := filepath.Rel(a.root, a.output); err != nil || rel == "." || !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		fatal("output must be outside the repository")
	}
	must(os.MkdirAll(a.output, 0700))
	a.load()
	a.fingerprint = a.hash()
	manifest := filepath.Join(a.output, "fingerprint")
	if old, err := os.ReadFile(manifest); err == nil && string(old) != a.fingerprint {
		fatal("source changed since this report; choose a new output directory")
	}
	must(os.WriteFile(manifest, []byte(a.fingerprint), 0600))
	if old, err := os.ReadFile(filepath.Join(a.output, "results.json")); err == nil {
		must(json.Unmarshal(old, &a.results))
	}
	// Isolation must be explicitly selected by the caller (usually copy mode).
	a.require("CHECK_ISOLATION: OK", nil, "check-isolation")
	a.require("PREFLIGHT: OK", nil, "preflight")
	a.require("RESULT: PASS", nil, "run", ".", ".*")
	list := a.require("LIST: OK", nil, "tests", ".")
	var tests []string
	selected, err := regexp.Compile(a.run)
	must(err)
	for line := range strings.SplitSeq(list, "\n") {
		if strings.HasPrefix(line, "Test") && !strings.ContainsAny(line, " \t") && selected.MatchString(line) {
			tests = append(tests, line)
		}
	}
	a.writeReport("IN_PROGRESS", len(tests))
	for _, name := range tests {
		if slices.ContainsFunc(a.results, func(r testResult) bool { return r.Test == name }) {
			continue
		}
		if a.hash() != a.fingerprint {
			fatal("source or tests changed during audit")
		}
		r := a.runTest(name)
		a.results = append(a.results, r)
		a.writeReport("IN_PROGRESS", len(tests))
		fmt.Printf("AUDIT: %s %s (%d mutations; %d/%d tests)\n", name, r.Status, len(r.Mutations), len(a.results), len(tests))
	}
	clean := a.call(nil, "check-clean")
	if !strings.Contains(clean, "CHECK_CLEAN: CLEAN") && a.hash() != a.fingerprint {
		fatal("audit left production or test mutations behind:\n%s", clean)
	}
	if a.hash() != a.fingerprint {
		fatal("source changed during audit")
	}
	a.writeReport("COMPLETE", len(tests))
	fmt.Printf("REPORT: %s\n", filepath.Join(a.output, "report.md"))
}

func (a *audit) load() {
	data, err := exec.Command("go", "list", "-json", ".").Output()
	must(err)
	var p struct{ GoFiles, TestGoFiles, XTestGoFiles []string }
	must(json.Unmarshal(data, &p))
	for _, name := range append(append(p.GoFiles, p.TestGoFiles...), p.XTestGoFiles...) {
		b, err := os.ReadFile(name)
		must(err)
		f, err := parser.ParseFile(a.fset, name, b, parser.ParseComments)
		must(err)
		if ast.IsGenerated(f) {
			continue
		}
		a.files = append(a.files, &sourceFile{name, b, f})
		if strings.HasSuffix(name, "_test.go") {
			for _, d := range f.Decls {
				if fn, ok := d.(*ast.FuncDecl); ok && strings.HasPrefix(fn.Name.Name, "Test") {
					a.testSource[fn.Name.Name], a.testFiles[fn.Name.Name] = fn, name
				}
			}
		}
	}
}

func (a *audit) hash() string {
	h := sha256.New()
	for _, f := range a.files {
		b, err := os.ReadFile(f.Name)
		must(err)
		fmt.Fprintf(h, "%s\x00", f.Name)
		h.Write(b)
	}
	for _, name := range []string{"go.mod", "go.sum"} {
		b, err := os.ReadFile(name)
		must(err)
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (a *audit) runTest(name string) testResult {
	r := testResult{Test: name, File: a.testFiles[name]}
	fn := a.testSource[name]
	if fn != nil {
		r.Line = a.fset.Position(fn.Pos()).Line
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if s, ok := n.(*ast.SelectorExpr); ok {
				switch s.Sel.Name {
				case "Error", "Errorf", "Fatal", "Fatalf", "Fail", "FailNow":
					r.Assertions = append(r.Assertions, a.fset.Position(s.Pos()).Line)
				}
			}
			return true
		})
	}
	re := "^" + regexp.QuoteMeta(name) + "$"
	baseline := a.call(nil, "run", ".", re)
	r.BaselineLog = a.save(name+"-baseline.log", baseline)
	if verdict(baseline) != "PASS" {
		r.Status, r.Reason = "UNAUDITABLE", "baseline "+verdict(baseline)
		return r
	}
	cover := a.call(nil, "cover", ".", re)
	a.save(name+"-coverage.log", cover)
	if !strings.Contains(cover, "COVER: OK") {
		r.Status, r.Reason = "UNAUDITABLE", "coverage failed"
		return r
	}
	type span struct{ lo, hi int }
	covered := map[string][]span{}
	for line := range strings.SplitSeq(cover, "\n") {
		var n int
		if _, err := fmt.Sscanf(line, "COVERED_STATEMENTS: %d", &n); err == nil {
			r.CoveredStatements = n
		}
		fields := strings.Fields(line)
		if len(fields) != 4 || fields[0] != "COVERED" {
			continue
		}
		var lo, hi int
		if _, err := fmt.Sscanf(fields[2], "%d-%d", &lo, &hi); err == nil {
			f := filepath.Base(fields[1])
			covered[f] = append(covered[f], span{lo, hi})
		}
	}
	if r.CoveredStatements == 0 {
		r.Status, r.Reason = "ZERO_PRODUCTION_COVERAGE", "fixture-only or no production statements exercised"
		return r
	}
	candidates := a.candidates()
	candidates = slices.DeleteFunc(candidates, func(m mutation) bool {
		return !slices.ContainsFunc(covered[m.File], func(s span) bool { return m.Line >= s.lo && m.Line <= s.hi })
	})
	// Prioritize the named subject and direct calls, not shared setup functions.
	score := func(m mutation) int {
		n, f := strings.ToLower(name), strings.ToLower(m.Function)
		s := 0
		if subjects[name] == m.Function {
			s += 1000
		}
		if strings.Contains(n, f) {
			s += 500 + len(f)
		}
		if fn != nil {
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				if c, ok := node.(*ast.CallExpr); ok {
					switch fun := c.Fun.(type) {
					case *ast.Ident:
						if fun.Name == m.Function {
							s += 50
						}
					case *ast.SelectorExpr:
						if fun.Sel.Name == m.Function {
							s += 50
						}
					}
				}
				return true
			})
		}
		if m.Operator == "M0" {
			s += 20
		}
		return s
	}
	slices.SortStableFunc(candidates, func(x, y mutation) int { return score(y) - score(x) })
	// One stub first, followed by behavioral perturbations rather than repeatedly
	// stubbing shared setup. Full candidate mode retains every generated stub.
	if a.max > 0 && len(candidates) > 0 {
		subject := candidates[0].Function
		slices.SortStableFunc(candidates, func(x, y mutation) int {
			sx, sy := score(x), score(y)
			if x.Function == subject && x.Operator == "M0" {
				sx += 10000
			}
			if y.Function == subject && y.Operator == "M0" {
				sy += 10000
			}
			if x.Operator == "M0" && x.Function != subject {
				sx -= 20000
			}
			if y.Operator == "M0" && y.Function != subject {
				sy -= 20000
			}
			return sy - sx
		})
	}
	r.Candidates = len(candidates)
	valid := 0
	for i, m := range candidates {
		if a.max > 0 && valid >= a.max {
			break
		}
		if a.hash() != a.fingerprint {
			fatal("source changed during audit")
		}
		a.require("SNAPSHOT:", nil, "snapshot", m.File)
		b, err := os.ReadFile(m.File)
		must(err)
		mutated := append([]byte(nil), b[:m.Start]...)
		mutated = append(mutated, m.After...)
		mutated = append(mutated, b[m.End:]...)
		prefix := fmt.Sprintf("%s-%04d", name, i)
		replacement := filepath.Join(a.output, prefix+".go")
		must(os.WriteFile(replacement, mutated, 0600))
		overlay := filepath.Join(a.output, prefix+".overlay.json")
		o, err := json.Marshal(map[string]any{"Replace": map[string]string{filepath.Join(a.root, m.File): replacement}})
		must(err)
		must(os.WriteFile(overlay, o, 0600))
		flags := strings.TrimSpace(os.Getenv("GOFLAGS") + " -overlay=" + overlay)
		out := a.call([]string{"GOFLAGS=" + flags}, "run", ".", re)
		m.Result, m.Log = verdict(out), a.save(prefix+".log", out)
		a.require("RESTORE: OK", nil, "restore", m.File)
		if m.Result == "TEST_FILES_MODIFIED" {
			fatal("test guard failed")
		}
		r.Mutations = append(r.Mutations, m)
		if m.Result != "BUILD_FAILED" && m.Result != "NO_TESTS_RUN" {
			valid++
		}
		fmt.Printf("  %s %s:%d %s RESULT: %s\n", name, m.File, m.Line, m.Operator, m.Result)
	}
	r.Status = "AUDITED"
	if len(r.Mutations) == 0 {
		r.Status, r.Reason = "UNAUDITABLE", "no generated candidates on covered statements"
	}
	return r
}

func (a *audit) candidates() []mutation {
	var ms []mutation
	for _, f := range a.files {
		if strings.HasSuffix(f.Name, "_test.go") {
			continue
		}
		for _, decl := range f.AST.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || len(fn.Body.List) == 0 {
				continue
			}
			add := func(op string, node ast.Node, after string) {
				start, end := a.fset.Position(node.Pos()), a.fset.Position(node.End())
				ms = append(ms, mutation{File: f.Name, Function: fn.Name.Name, Operator: op, Line: start.Line,
					Start: start.Offset, End: end.Offset, Before: string(f.Data[start.Offset:end.Offset]), After: after})
			}
			var stub strings.Builder
			var values []string
			if fn.Type.Results != nil {
				for _, field := range fn.Type.Results.List {
					n := max(1, len(field.Names))
					for range n {
						name := fmt.Sprintf("__mutationZero%d", len(values))
						fmt.Fprintf(&stub, "var %s %s\n", name, render(field.Type))
						values = append(values, name)
					}
				}
			}
			fmt.Fprintf(&stub, "return %s\n", strings.Join(values, ", "))
			p := a.fset.Position(fn.Body.List[0].Pos())
			ms = append(ms, mutation{File: f.Name, Function: fn.Name.Name, Operator: "M0",
				Line: p.Line, Start: p.Offset, End: p.Offset, After: stub.String()})
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				switch n := node.(type) {
				case *ast.FuncLit:
					// Callback behavior deserves explicit candidates, but its return
					// signature differs from the enclosing function: no synthetic stub.
					return true
				case *ast.CallExpr:
					if s, ok := n.Fun.(*ast.SelectorExpr); ok {
						if id, ok := s.X.(*ast.Ident); ok && (id.Name == "log" || id.Name == "slog") {
							return false
						}
					}
				case *ast.IfStmt:
					add("M1", n.Cond, "!("+render(n.Cond)+")")
				case *ast.ReturnStmt:
					for _, v := range n.Results {
						if lit, ok := v.(*ast.BasicLit); ok {
							switch lit.Kind {
							case token.STRING:
								s, err := strconv.Unquote(lit.Value)
								if err == nil {
									add("M3", lit, strconv.Quote(s+"__mutant"))
								}
							case token.INT:
								add("M3", lit, "("+lit.Value+" + 1)")
							}
						}
						if id, ok := v.(*ast.Ident); ok && (id.Name == "true" || id.Name == "false") {
							add("M2", id, "!("+id.Name+")")
						}
					}
				case *ast.BinaryExpr:
					replacement := map[token.Token]string{token.LSS: "<=", token.LEQ: "<", token.GTR: ">=", token.GEQ: ">", token.LAND: "||", token.LOR: "&&"}
					if op := replacement[n.Op]; op != "" {
						p := a.fset.Position(n.OpPos)
						ms = append(ms, mutation{File: f.Name, Function: fn.Name.Name, Operator: "M5",
							Line: p.Line, Start: p.Offset, End: p.Offset + len(n.Op.String()), Before: n.Op.String(), After: op})
					}
				}
				return true
			})
		}
	}
	return ms
}

func (a *audit) call(env []string, args ...string) string {
	cmd := exec.Command("bash", append([]string{a.helper}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil && len(out) == 0 {
		fatal("helper failed: %v", err)
	}
	return string(out)
}

func (a *audit) require(want string, env []string, args ...string) string {
	out := a.call(env, args...)
	if !strings.Contains(out, want) {
		fatal("helper %v: expected %s\n%s", args, want, out)
	}
	return out
}

func (a *audit) save(name, s string) string {
	must(os.WriteFile(filepath.Join(a.output, name), []byte(s), 0600))
	return name
}

func (a *audit) writeReport(status string, total int) {
	b, err := json.MarshalIndent(a.results, "", "  ")
	must(err)
	must(os.WriteFile(filepath.Join(a.output, "results.json"), b, 0600))
	var report strings.Builder
	fmt.Fprintf(&report, "# Sequential mutation audit\n\nStatus: %s\n\nSource SHA-256: `%s`\n\nTests accounted for: %d/%d. Per-test mutation limit: %d (0 = all generated covered candidates).\n\n", status, a.fingerprint, len(a.results), total, a.max)
	report.WriteString("Production overlays only; baseline source and tests are never modified. A survivor is relative to the named test, not automatically the entire suite. Automated candidates require semantic-equivalence review; panic/timeout kills are weaker than assertion kills. This is exhaustive test inventory, not proof against all possible defects.\n\n")
	report.WriteString("| Test | Status | Covered statements | Candidates | Mutations | Pass/survive | Fail | Panic | Timeout | Invalid/skip |\n|---|---|---:|---:|---:|---:|---:|---:|---:|---:|\n")
	var applied, survivors, fails, panics, timeouts, invalid int
	for _, r := range a.results {
		counts := map[string]int{}
		for _, m := range r.Mutations {
			counts[m.Result]++
		}
		bad := len(r.Mutations) - counts["PASS"] - counts["FAIL"] - counts["PANIC"] - counts["TIMEOUT"]
		applied += len(r.Mutations)
		survivors += counts["PASS"]
		fails += counts["FAIL"]
		panics += counts["PANIC"]
		timeouts += counts["TIMEOUT"]
		invalid += bad
		fmt.Fprintf(&report, "| %s | %s %s | %d | %d | %d | %d | %d | %d | %d | %d |\n", r.Test, r.Status, r.Reason, r.CoveredStatements, r.Candidates, len(r.Mutations), counts["PASS"], counts["FAIL"], counts["PANIC"], counts["TIMEOUT"], bad)
	}
	fmt.Fprintf(&report, "\nTotals: %d applied; %d survivors; %d assertion failures; %d panics; %d timeouts; %d invalid/skipped.\n", applied, survivors, fails, panics, timeouts, invalid)
	for _, r := range a.results {
		for i, m := range r.Mutations {
			if m.Result != "PASS" {
				continue
			}
			fmt.Fprintf(&report, "\n## Survivor: %s / %d\n\nTarget: `%s:%d`, `%s`, operator `%s`. Baseline covered statements: %d.\n\nBefore:\n```go\n%s\n```\nAfter:\n```go\n%s\n```\n\nReproduce with retained overlay:\n```sh\nGOFLAGS='-overlay=%s' bash '%s' run . '^%s$'\n# RESULT: %s\n```\nLog: `%s`.\n", r.Test, i, m.File, m.Line, m.Function, m.Operator, r.CoveredStatements, m.Before, m.After, filepath.Join(a.output, strings.TrimSuffix(m.Log, ".log")+".overlay.json"), a.helper, r.Test, m.Result, m.Log)
		}
	}
	must(os.WriteFile(filepath.Join(a.output, "report.md"), []byte(report.String()), 0600))
	var lines bytes.Buffer
	for _, r := range a.results {
		b, err := json.Marshal(r)
		must(err)
		lines.Write(b)
		lines.WriteByte('\n')
	}
	must(os.WriteFile(filepath.Join(a.output, "root.jsonl"), lines.Bytes(), 0600))
}

func verdict(s string) string {
	v := regexp.MustCompile(`(?m)^RESULT: ([A-Z_]+)`).FindStringSubmatch(s)
	if len(v) != 2 {
		fatal("helper produced no verdict:\n%s", s)
	}
	return v[1]
}

func render(n ast.Node) string {
	var b bytes.Buffer
	must(printer.Fprint(&b, token.NewFileSet(), n))
	return b.String()
}

func must(err error) {
	if err != nil {
		fatal("%v", err)
	}
}
func fatal(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...); os.Exit(1) }
