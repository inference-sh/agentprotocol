package history

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Pattern maps a result line back to the check ID the runner recorded it
// under. Patterns are read from the runner's source, so a skip ("acp: stop
// hook not fired — …") and a pass ("acp: stop hook fired") of one check get
// the same ID, which their lines alone do not give them.
type Pattern struct {
	re *regexp.Regexp
	// id is the ID's parts.
	id []idPart
}

// idPart is a literal, the index of the capture that fills it (-1 for an
// expression no capture fills), or hookID(prefix, label) with the captures
// of prefix and label.
type idPart struct {
	lit  string
	arg  int
	hook *[2]int
}

var verbRe = regexp.MustCompile(`%[-+# 0-9.]*[a-zA-Z]`)

// Patterns reads every r.pass, r.fail, r.skip and r.finding call in the
// runner's .go files under dir whose message is a literal or a fmt.Sprintf
// of one.
func Patterns(dir string) ([]Pattern, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, err
	}
	var out []Pattern
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			return nil, err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "pass", "fail", "skip", "finding":
			default:
				return true
			}
			if p, ok := pattern(call.Args[0], call.Args[1]); ok {
				out = append(out, p)
			}
			return true
		})
	}
	return out, nil
}

func pattern(idExpr, msgExpr ast.Expr) (Pattern, bool) {
	var format string
	var args []ast.Expr
	switch m := msgExpr.(type) {
	case *ast.BasicLit:
		format, _ = strconv.Unquote(m.Value)
	case *ast.CallExpr:
		sel, ok := m.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Sprintf" || len(m.Args) == 0 {
			return Pattern{}, false
		}
		lit, ok := m.Args[0].(*ast.BasicLit)
		if !ok {
			return Pattern{}, false
		}
		format, _ = strconv.Unquote(lit.Value)
		args = m.Args[1:]
	case *ast.BinaryExpr:
		// "x" + name + " installed": each operand that is not a literal is
		// a capture.
		var flatten func(e ast.Expr) bool
		flatten = func(e ast.Expr) bool {
			if b, ok := e.(*ast.BinaryExpr); ok {
				return b.Op == token.ADD && flatten(b.X) && flatten(b.Y)
			}
			if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				s, err := strconv.Unquote(lit.Value)
				format += strings.ReplaceAll(s, "%", "%%")
				return err == nil
			}
			format += "%s"
			args = append(args, e)
			return true
		}
		if !flatten(m) {
			return Pattern{}, false
		}
	default:
		return Pattern{}, false
	}
	if format == "" {
		return Pattern{}, false
	}
	// The message regexp: each verb a lazy capture, the rest literal.
	var re strings.Builder
	re.WriteString("^")
	last := 0
	for _, loc := range verbRe.FindAllStringIndex(format, -1) {
		re.WriteString(regexp.QuoteMeta(format[last:loc[0]]))
		if format[loc[0]:loc[1]] == "%%" {
			re.WriteString("%")
		} else {
			re.WriteString("(.*?)")
		}
		last = loc[1]
	}
	re.WriteString(regexp.QuoteMeta(format[last:]))
	re.WriteString("$")
	compiled, err := regexp.Compile(re.String())
	if err != nil {
		return Pattern{}, false
	}
	p := Pattern{re: compiled}
	argIndex := func(e ast.Expr) int {
		want := exprString(e)
		for i, a := range args {
			if exprString(a) == want {
				return i
			}
		}
		return -1
	}
	var walk func(e ast.Expr) bool
	walk = func(e ast.Expr) bool {
		switch x := e.(type) {
		case *ast.BasicLit:
			s, err := strconv.Unquote(x.Value)
			if err != nil {
				return false
			}
			p.id = append(p.id, idPart{lit: s, arg: -1})
		case *ast.BinaryExpr:
			if x.Op != token.ADD {
				return false
			}
			return walk(x.X) && walk(x.Y)
		case *ast.CallExpr:
			// probeID(label) and the like: the argument names the part.
			if exprString(x.Fun) == "hookID" && len(x.Args) == 2 {
				p.id = append(p.id, idPart{arg: -1, hook: &[2]int{argIndex(x.Args[0]), argIndex(x.Args[1])}})
			} else if len(x.Args) == 1 {
				p.id = append(p.id, idPart{arg: argIndex(x.Args[0])})
			} else {
				p.id = append(p.id, idPart{arg: -1})
			}
		default:
			p.id = append(p.id, idPart{arg: argIndex(e)})
		}
		return true
	}
	if !walk(idExpr) {
		return Pattern{}, false
	}
	return p, true
}

func exprString(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return exprString(x.X) + "." + x.Sel.Name
	case *ast.BasicLit:
		return x.Value
	case *ast.CallExpr:
		var b strings.Builder
		b.WriteString(exprString(x.Fun) + "(")
		for i, a := range x.Args {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(exprString(a))
		}
		b.WriteString(")")
		return b.String()
	}
	return "?"
}

// Match returns the check ID msg was recorded under, split from its
// ":answer" suffix ("hook.stop", "known-missing"), and whether a pattern
// matched. The longest literal format wins when several match.
func Match(patterns []Pattern, msg string) (id, answer string, ok bool) {
	best, bestLen := "", -1
	for _, p := range patterns {
		m := p.re.FindStringSubmatch(msg)
		if m == nil {
			continue
		}
		if n := len(p.re.String()); n > bestLen {
			best, bestLen = p.render(m[1:]), n
		}
	}
	id, answer, _ = strings.Cut(best, ":")
	return id, answer, bestLen >= 0
}

func (p Pattern) render(caps []string) string {
	capture := func(i int) string {
		if i < 0 || i >= len(caps) {
			return "*"
		}
		return caps[i]
	}
	var b strings.Builder
	for _, part := range p.id {
		if part.hook != nil {
			prefix, label := strings.TrimSpace(capture(part.hook[0])), capture(part.hook[1])
			// "%s%s" captures lazily, so "belt post-tool" lands whole in
			// label; labels have no spaces.
			if i := strings.LastIndex(label, " "); i >= 0 {
				prefix, label = strings.TrimSpace(prefix+" "+label[:i]), label[i+1:]
			}
			if prefix != "" {
				b.WriteString(slug(prefix) + "-")
			}
			b.WriteString("hook." + label)
		} else if part.arg == -1 && part.lit != "" {
			b.WriteString(part.lit)
		} else {
			b.WriteString(capture(part.arg))
		}
	}
	return b.String()
}

// slug is the runner's slug: lower case, runs of anything but [a-z0-9._]
// one dash.
func slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	dash := false
	for _, c := range s {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' {
			b.WriteRune(c)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}
