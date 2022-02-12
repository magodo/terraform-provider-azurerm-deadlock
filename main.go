package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/bflad/tfproviderlint/helper/astutils"
	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/packages"
)

const (
	rootPkgPath    = "github.com/hashicorp/terraform-provider-azurerm"
	lockModulePath = "internal/locks"
)

type Locksites map[Pos][]Locksite

func (ls Locksites) AddLocksite(site Locksite) {
	if _, ok := ls[site.EnclosingFuncPos]; !ok {
		ls[site.EnclosingFuncPos] = []Locksite{}
	}
	ls[site.EnclosingFuncPos] = append(ls[site.EnclosingFuncPos], site)
}

type Pos string

type Poses []Pos

func (ps Poses) Len() int           { return len(ps) }
func (ps Poses) Less(i, j int) bool { return ps[i] < ps[j] }
func (ps Poses) Swap(i, j int)      { ps[i], ps[j] = ps[j], ps[i] }

type Locksite struct {
	ResourceType     string
	EnclosingFuncPos Pos
	Pkg              *packages.Package
	// The position of the locks call
	Pos token.Pos
}

type LockPair string

func NewLockPair(rt1, rt2 string) LockPair {
	return LockPair(rt1 + "->" + rt2)
}

func (lp LockPair) Reverse() LockPair {
	segs := strings.Split(string(lp), "->")
	return NewLockPair(segs[1], segs[0])
}

type LockPairRecord map[LockPair]Poses

func NewLockPairRecord(locksites Locksites) LockPairRecord {
	result := LockPairRecord{}
	for fdecl, locks := range locksites {
		for i := 0; i < len(locks)-1; i++ {
			for j := i + 1; j < len(locks); j++ {
				lp := NewLockPair(locks[i].ResourceType, locks[j].ResourceType)
				if _, ok := result[lp]; !ok {
					result[lp] = Poses{}
				}
				result[lp] = append(result[lp], fdecl)
			}
		}
	}

	// Sort the fdecl positions
	for lp, poses := range result {
		sort.Sort(poses)
		result[lp] = poses
	}
	return result
}

func main() {
	cfg := &packages.Config{Mode: packages.LoadSyntax}
	pkgs, err := packages.Load(cfg, os.Args[1:]...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load: %v\n", err)
		os.Exit(1)
	}
	if packages.PrintErrors(pkgs) > 0 {
		os.Exit(1)
	}

	// Collect all value specs that define string literal (naively)
	pkgconststrings := map[types.Object]string{}
	for _, pkg := range pkgs {
		for _, f := range pkg.Syntax {
			ast.Inspect(f, func(node ast.Node) bool {
				spec, ok := node.(*ast.ValueSpec)
				if !ok {
					return true
				}
				if len(spec.Names) != len(spec.Values) {
					return true
				}
				for i := range spec.Names {
					n, v := spec.Names[i], spec.Values[i]
					vb, ok := v.(*ast.BasicLit)
					if !ok {
						continue
					}
					if vb.Kind != token.STRING {
						continue
					}
					pkgconststrings[pkg.TypesInfo.Defs[n]], _ = strconv.Unquote(vb.Value)
				}
				return true
			})
		}
	}

	// Collect lock call expressions in the same scope, in order
	locks := Locksites{}
	pwd, _ := os.Getwd()
	for _, pkg := range pkgs {
		for _, f := range pkg.Syntax {
			ast.Inspect(f, func(node ast.Node) bool {
				callExpr, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				if !isLocksCallExpr(callExpr.Fun, pkg.TypesInfo) {
					return true
				}

				var resType string
				switch rt := callExpr.Args[1].(type) {
				case *ast.SelectorExpr:
					name, ok := pkgconststrings[pkg.TypesInfo.Uses[rt.Sel]]
					if !ok {
						return false
					}
					resType = name
				case *ast.Ident:
					name, ok := pkgconststrings[pkg.TypesInfo.Uses[rt]]
					if !ok {
						return false
					}
					resType = name
				case *ast.BasicLit:
					resType, _ = strconv.Unquote(rt.Value)
				default:
					panic(fmt.Sprintf("%v: unhandled type of resource type being locked", rt.Pos()))
				}

				var enclosingFuncDecl *ast.FuncDecl
				path, _ := astutil.PathEnclosingInterval(f, callExpr.Pos(), callExpr.Pos())
				for _, pnode := range path {
					fdecl, ok := pnode.(*ast.FuncDecl)
					if !ok {
						continue
					}
					enclosingFuncDecl = fdecl
					break
				}
				if enclosingFuncDecl == nil {
					panic(fmt.Sprintf("failed to find the enclosing function declaration for %s", pkg.Fset.Position(callExpr.Pos()).String()))
				}

				site := Locksite{
					ResourceType:     resType,
					Pkg:              pkg,
					EnclosingFuncPos: Pos(strings.TrimPrefix(pkg.Fset.Position(enclosingFuncDecl.Pos()).String(), pwd+string(filepath.Separator))),
					Pos:              callExpr.Pos(),
				}
				locks.AddLocksite(site)

				return false
			})
		}
	}

	lpr := NewLockPairRecord(locks)
	keys := []string{}
	for lp := range lpr {
		keys = append(keys, string(lp))
	}
	sort.Strings(keys)

	processed := map[LockPair]bool{}
	messages := []string{}
	for _, key := range keys {
		lp := LockPair(key)
		if processed[lp] {
			continue
		}
		processed[lp] = true

		pos1s := lpr[lp]
		pos2s, ok := lpr[lp.Reverse()]
		if !ok {
			continue
		}
		processed[lp.Reverse()] = true

		var pos1msg, pos2msg []string
		for _, pos := range pos1s {
			pos1msg = append(pos1msg, "\t- "+string(pos))
		}
		for _, pos := range pos2s {
			pos2msg = append(pos2msg, "\t- "+string(pos))
		}

		msg := fmt.Sprintf(`Potential deadlock:
  %s:
%s
  %s:
%s
`, lp, strings.Join(pos1msg, "\n"), lp.Reverse(), strings.Join(pos2msg, "\n"))

		messages = append(messages, msg)
	}

	fmt.Println(strings.Join(messages, "\n"))
}

func isLocksCallExpr(e ast.Expr, info *types.Info) bool {
	return astutils.IsModulePackageFunc(e, info, rootPkgPath, lockModulePath, "ByName") || astutils.IsModulePackageFunc(e, info, rootPkgPath, lockModulePath, "MultipleByName")
}
