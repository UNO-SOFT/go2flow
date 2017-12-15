package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"html"
	"io"
	"log"
	"os"
	"strings"
	"sync/atomic"

	wordwrap "github.com/mitchellh/go-wordwrap"
	"github.com/pkg/errors"
)

func main() {
	if err := Main(); err != nil {
		log.Fatal(err)
	}
}

func Main() error {
	flag.Parse()
	fn := flag.Arg(0)
	fh := os.Stdin
	if fn != "" && fn != "-" {
		var err error
		if fh, err = os.Open(fn); err != nil {
			return err
		}
	}
	var buf bytes.Buffer
	_, err := io.Copy(&buf, fh)
	fh.Close()
	if err != nil {
		return err
	}
	src := buf.String()
	buf.Reset()

	r := io.Reader(strings.NewReader(src))
	if !(strings.HasPrefix(src, "package ") || strings.Contains(src, "\npackage ")) {
		r = io.MultiReader(
			strings.NewReader(`package main
func do(s string) error { return nil }
func Q(s string) bool { return true }
func main() {`),
			strings.NewReader("\n}\n"),
		)
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, fn, r, parser.AllErrors)
	if err != nil {
		return errors.Wrap(err, "parse")
	}
	for _, d := range f.Decls {
		fd, _ := d.(*ast.FuncDecl)
		if fd == nil || fd.Name.Name != "main" {
			continue
		}
		stmts, err := parseStmtList(fd.Body.List)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "/*\n")
		if err = Print(os.Stdout, "# ", stmts); err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "*/\n")
		if err = PrintGraph(os.Stdout, stmts); err != nil {
			return err
		}
		break
	}
	return nil
}

func PrintGraph(w io.Writer, stmts []Stmt) error {
	bw := bufio.NewWriter(w)
	defer bw.Flush()
	bw.WriteString("digraph G {\n")
	if err := printGraphStmts(bw, 0, stmts); err != nil {
		return err
	}
	bw.WriteString("}\n")
	return bw.Flush()
}

var nodeCnt int32

func printGraphStmts(w io.Writer, level int, stmts []Stmt) error {
	bw := bufio.NewWriter(w)
	defer bw.Flush()
	var prev string
	prefix := strings.Repeat("  ", level)
	for _, stmt := range stmts {
		cnt := atomic.AddInt32(&nodeCnt, 1)
		name := fmt.Sprintf("N%03d", cnt)
		switch x := stmt.(type) {
		case Do:
			fmt.Fprintf(bw, "%s  %s [shape=box label=%q]\n", prefix, name, esc(x.What))
			if prev != "" {
				fmt.Fprintf(bw, "%s  %s -> %s\n", prefix, prev, name)
			}
			prev = name
		case If:
			fmt.Fprintf(bw, "%s  %s [shape=diamond label=%q]\n", prefix, name, esc(x.Cond))
			if prev != "" {
				fmt.Fprintf(bw, "%s  %s -> %s\n", prefix, prev, name)
			}
			for k, xs := range map[string][]Stmt{"igen": x.Then, "nem": x.Else} {
				if len(xs) == 0 {
					continue
				}
				nxtName := fmt.Sprintf("N%03d", atomic.LoadInt32(&nodeCnt)+1)
				fmt.Fprintf(bw, "%s  %s -> %s [label=%s]\n", prefix, name, nxtName, k)
				subName := fmt.Sprintf("%s_%s", name, k)
				fmt.Fprintf(bw, "%s  subgraph %s {\n", prefix, subName)
				if err := printGraphStmts(w, level+1, xs); err != nil {
					return err
				}
				fmt.Fprintf(bw, "%s  }\n", prefix)
			}
			prev = name

		default:
			return errors.Errorf("unknown Stmt %T %+v", x, x)
		}
	}
	return bw.Flush()
}

func Print(w io.Writer, prefix string, stmts []Stmt) error {
	for _, stmt := range stmts {
		if err := stmt.Print(w, prefix); err != nil {
			return err
		}
	}
	return nil
}

type Stmt interface {
	Print(w io.Writer, prefix string) error
}
type Do struct {
	What string
}

func (d Do) Print(w io.Writer, prefix string) error {
	_, err := fmt.Fprintf(w, "%sDO %s\n", prefix, d.What)
	return err
}

type If struct {
	Cond string
	Then []Stmt
	Else []Stmt
}

func (i If) Print(w io.Writer, prefix string) error {
	_, err := fmt.Fprintf(w, "%sIF %s THEN\n", prefix, i.Cond)
	if err != nil {
		return err
	}
	if err = Print(w, prefix+"  ", i.Then); err != nil {
		return err
	}
	return Print(w, prefix+"  ", i.Else)
}

func parseStmtList(stmtList []ast.Stmt) ([]Stmt, error) {
	stmts := make([]Stmt, 0, len(stmtList))
	for _, stmt := range stmtList {
		switch x := stmt.(type) {
		case *ast.ExprStmt:
			if c, _ := x.X.(*ast.CallExpr); c == nil || len(c.Args) != 1 {
				return stmts, errors.Errorf("unknown call %T %+v (wanted with one arg)", x.X, x.X)
			} else if bl, _ := c.Args[0].(*ast.BasicLit); bl == nil || bl.Kind != token.STRING {
				return stmts, errors.Errorf("unknown arg %T %+v (wanted string)", c.Args[0], c.Args[0])
			} else {
				stmts = append(stmts, Do{What: bl.Value})
			}
		case *ast.IfStmt:
			if c, _ := x.Cond.(*ast.CallExpr); c == nil || len(c.Args) != 1 {
				return stmts, errors.Errorf("unknown if expr %T %+v (wanted with one arg)", x.Cond, x.Cond)
			} else if bl, _ := c.Args[0].(*ast.BasicLit); bl == nil || bl.Kind != token.STRING {
				return stmts, errors.Errorf("unknown cond arg %T %+v (wanted string)", c.Args[0], c.Args[0])
			} else {
				ifs := If{Cond: bl.Value}
				if x.Body != nil && len(x.Body.List) != 0 {
					var err error
					if ifs.Then, err = parseStmtList(x.Body.List); err != nil {
						return stmts, err
					}
				}
				if x.Else != nil {
					if bs, _ := x.Else.(*ast.BlockStmt); bs != nil && len(bs.List) != 0 {
						var err error
						if ifs.Else, err = parseStmtList(bs.List); err != nil {
							return stmts, err
						}
					}
				}
				stmts = append(stmts, ifs)
			}
		default:
			return stmts, errors.Errorf("unknown statement %T %+v", x, x)
		}
	}
	return stmts, nil
}

func esc(s string) string {
	if s == "" {
		return s
	}
	if len(s) < 2 {
		return html.EscapeString(s)
	}
	if !(strings.Contains(s, "\n") || strings.Contains(s, "<br>")) {
		s = wordwrap.WrapString(s, 30)
	}
	//s = strings.Replace(s, "\n", "\\n", -1)
	if s[0] == '`' && s[len(s)-1] == '`' {
		return html.EscapeString(s[1 : len(s)-1])
	}

	return html.EscapeString(s)
}
