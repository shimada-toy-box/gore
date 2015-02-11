package main

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"

	"go/ast"
	"go/parser"
	"go/printer"
	"go/scanner"
	"go/token"
	"golang.org/x/tools/go/ast/astutil"
	_ "golang.org/x/tools/go/gcimporter"
	"golang.org/x/tools/go/types"

	"github.com/peterh/liner"
)

const appName = "gore"

var debug = false

const (
	promptDefault  = "gore> "
	promptContinue = "..... "
)

func debugf(format string, args ...interface{}) {
	if !debug {
		return
	}

	_, file, line, ok := runtime.Caller(1)
	if ok {
		format = fmt.Sprintf("%s:%d %s", filepath.Base(file), line, format)
	}

	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

func main() {
	s := NewSession()

	rl := liner.NewLiner()
	defer rl.Close()

	in := ""
	prompt := promptDefault

	// TODO: set up completion for:
	// - imports
	// - methods/fields using gocode?

	for {
		line, err := rl.Prompt(prompt)
		if err == io.EOF {
			if in != "" {
				// cancel line continuation
				rl.AppendHistory(in)
				in = ""
				prompt = promptDefault
				fmt.Println()
				continue
			} else {
				break
			}
		} else if err != nil {
			fmt.Fprintf(os.Stderr, "fatal: %s", err)
			os.Exit(1)
		}

		if in != "" {
			in = in + "\n" + line
		} else {
			in = line
		}

		err = s.Run(in)
		if err == ErrContinue {
			prompt = promptContinue
		} else {
			rl.AppendHistory(in)
			in = ""
			prompt = promptDefault
			if err != nil {
				fmt.Println(err)
			}
		}
	}
}

type Session struct {
	FilePath string
	File     *ast.File
	Fset     *token.FileSet

	Source *bytes.Buffer

	mainBody         *ast.BlockStmt
	storedBodyLength int
}

const initialSource = `
package main

import "fmt"

func p(xx ...interface{}) {
	for _, x := range xx {
		fmt.Printf("%#v\n", x)
	}
}

func main() {
}
`

func NewSession() *Session {
	var err error

	s := &Session{}
	s.Fset = token.NewFileSet()
	s.Source = bytes.NewBufferString(initialSource)

	// s.FilePath, err = tempFile()
	s.FilePath = "_tmp/session.go"
	if err != nil {
		panic(err)
	}

	s.File, err = parser.ParseFile(s.Fset, "session.go", initialSource, parser.Mode(0))
	if err != nil {
		panic(err)
	}

	mainFunc := s.File.Scope.Lookup("main").Decl.(*ast.FuncDecl)
	s.mainBody = mainFunc.Body

	return s
}

func (s *Session) BuildRunFile() error {
	s.Source = new(bytes.Buffer)
	printer.Fprint(s.Source, s.Fset, s.File)

	f, err := os.Create(s.FilePath)
	if err != nil {
		return err
	}

	_, err = f.Write(s.Source.Bytes())
	if err != nil {
		return err
	}

	return goRun(s.FilePath)
}

func tempFile() (string, error) {
	dir, err := ioutil.TempDir("", appName)
	if err != nil {
		return "", err
	}

	err = os.MkdirAll(dir, 0755)
	if err != nil {
		return "", err
	}

	return filepath.Join(dir, "session.go"), nil
}

func goRun(file string) error {
	debugf("go run %s", file)

	cmd := exec.Command("go", "run", file)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (s *Session) injectExpr(in string) error {
	expr, err := parser.ParseExpr(in)
	if err != nil {
		return err
	}

	normalizeNode(expr)

	stmt := &ast.ExprStmt{
		X: &ast.CallExpr{
			Fun:  ast.NewIdent("p"), // TODO remove this after evaluation
			Args: []ast.Expr{expr},
		},
	}

	s.appendStatements(stmt)

	return nil
}

func (s *Session) injectStmt(in string) error {
	src := fmt.Sprintf("package P; func F() { %s }", in)
	f, err := parser.ParseFile(s.Fset, "stmt.go", src, parser.Mode(0))
	if err != nil {
		return err
	}

	enclosingFunc := f.Scope.Lookup("F").Decl.(*ast.FuncDecl)
	s.appendStatements(enclosingFunc.Body.List...)

	return nil
}

func (s *Session) appendStatements(stmts ...ast.Stmt) {
	s.mainBody.List = append(s.mainBody.List, stmts...)
}

type Error string

const (
	ErrContinue Error = "<continue input>"
)

func (e Error) Error() string {
	return string(e)
}

func (s *Session) handleImport(in string) bool {
	// TODO make this ":import "?
	if !strings.HasPrefix(in, "import ") {
		return false
	}

	path := in[len("import "):]
	path = strings.Trim(path, `"`)

	astutil.AddImport(s.Fset, s.File, path)

	return true
}

var (
	rxDeclaredNotUsed = regexp.MustCompile(`^([a-zA-Z0-9_]+) declared but not used`)
	rxImportedNotUsed = regexp.MustCompile(`^(".+") imported but not used`)
)

// quickFixFile tries to fix the source AST so that it compiles well.
func (s *Session) quickFixFile() error {
	const maxAttempts = 10

	for i := 0; i < maxAttempts; i++ {
		_, err := types.Check("_quickfix", s.Fset, []*ast.File{s.File})
		if err == nil {
			break
		}

		debugf("quickFix :: err = %#v", err)

		if err, ok := err.(types.Error); ok && err.Soft {
			// Handle these situations:
			// - "%s declared but not used"
			// - "%q imported but not used"
			if m := rxDeclaredNotUsed.FindStringSubmatch(err.Msg); m != nil {
				ident := m[1]
				debugf("quickFix :: declared but not used -> %s", ident)
				// insert "_ = x" to supress "declared but not used" error
				// TODO: remove this statement after evaluation
				stmt := &ast.AssignStmt{
					Lhs: []ast.Expr{ast.NewIdent("_")},
					Tok: token.ASSIGN,
					Rhs: []ast.Expr{ast.NewIdent(ident)},
				}
				s.appendStatements(stmt)
			} else if m := rxImportedNotUsed.FindStringSubmatch(err.Msg); m != nil {
				path := m[1] // quoted string, but it's okay because this will be compared to ast.BasicLit.Value.
				debugf("quickFix :: imported but not used -> %s", path)

				for _, imp := range s.File.Imports {
					debugf("%s vs %s", imp.Path.Value, path)
					if imp.Path.Value == path {
						// make this import spec anonymous one
						imp.Name = ast.NewIdent("_")
						break
					}
				}
			} else {
				debugf("quickFix :: give up")
				break
			}
		} else {
			return err
		}
	}

	return nil
}

func (s *Session) clearQuickFix() {
	// make all import specs explicit (i.e. no "_").
	for _, imp := range s.File.Imports {
		imp.Name = nil
	}
}

func (s *Session) Run(in string) error {
	debugf("run >>> %q", in)

	s.clearQuickFix()

	imported := s.handleImport(in)

	if !imported {
		if err := s.injectExpr(in); err != nil {
			debugf("expr :: err = %s", err)

			err := s.injectStmt(in)
			if err != nil {
				debugf("stmt :: err = %s", err)

				if _, ok := err.(scanner.ErrorList); ok {
					return ErrContinue
				}
			}
		}
	}

	s.quickFixFile()

	err := s.BuildRunFile()

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			// if failed with status 2, remove the last statement
			if st, ok := exitErr.ProcessState.Sys().(syscall.WaitStatus); ok {
				if st.ExitStatus() == 2 {
					debugf("got exit status 2, popping out last input")
					s.RecallCode()
				}
			}
		}
	} else {
		s.RememberCode()
	}

	return err
}

// RememberCode stores current state of code so that it can be restored
// actually it saves the length of statements inside main()
func (s *Session) RememberCode() {
	s.storedBodyLength = len(s.mainBody.List)
}

func (s *Session) RecallCode() {
	s.mainBody.List = s.mainBody.List[0:s.storedBodyLength]
}

func normalizeNode(node ast.Node) {
	// TODO remove token.Pos information
}