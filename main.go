package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"strconv"

	"github.com/bflad/tfproviderlint/helper/astutils"
	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/packages"
)

const (
	rootPkgPath    = "github.com/hashicorp/terraform-provider-azurerm"
	lockModulePath = "internal/locks"
)

type Locksites map[*ast.FuncDecl][]Locksite

func (ls Locksites) AddLocksite(site Locksite) {
	if _, ok := ls[site.EnclosingFunc]; !ok {
		ls[site.EnclosingFunc] = []Locksite{}
	}
	ls[site.EnclosingFunc] = append(ls[site.EnclosingFunc], site)
}

type Locksite struct {
	ResourceType  string
	EnclosingFunc *ast.FuncDecl
	Pkg           *packages.Package
	// The position of the locks call
	Pos token.Pos
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
					ResourceType:  resType,
					Pkg:           pkg,
					EnclosingFunc: enclosingFuncDecl,
					Pos:           callExpr.Pos(),
				}
				locks.AddLocksite(site)

				return false
			})
		}
	}

	for enclosingFunc, _locks := range locks {
		for _, lock := range _locks {
			fmt.Printf("%v: %s\n", lock.Pkg.Fset.Position(enclosingFunc.Pos()).String(), lock.ResourceType)
		}
	}
}

func isLocksCallExpr(e ast.Expr, info *types.Info) bool {
	return astutils.IsModulePackageFunc(e, info, rootPkgPath, lockModulePath, "ByName") || astutils.IsModulePackageFunc(e, info, rootPkgPath, lockModulePath, "MultipleByName")
}
