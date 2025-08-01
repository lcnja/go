// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package typecheck

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/types"
	"cmd/internal/src"

	"fmt"
	"go/constant"
	"go/token"
)

// MakeDotArgs package all the arguments that match a ... T parameter into a []T.
func MakeDotArgs(pos src.XPos, typ *types.Type, args []ir.Node) ir.Node {
	if len(args) == 0 {
		return ir.NewNilExpr(pos, typ)
	}

	args = append([]ir.Node(nil), args...)
	lit := ir.NewCompLitExpr(pos, ir.OCOMPLIT, typ, args)
	lit.SetImplicit(true)

	n := Expr(lit)
	if n.Type() == nil {
		base.FatalfAt(pos, "mkdotargslice: typecheck failed")
	}
	return n
}

// FixVariadicCall rewrites calls to variadic functions to use an
// explicit ... argument if one is not already present.
func FixVariadicCall(call *ir.CallExpr) {
	fntype := call.Fun.Type()
	if !fntype.IsVariadic() || call.IsDDD {
		return
	}

	vi := fntype.NumParams() - 1
	vt := fntype.Param(vi).Type

	args := call.Args
	extra := args[vi:]
	slice := MakeDotArgs(call.Pos(), vt, extra)
	for i := range extra {
		extra[i] = nil // allow GC
	}

	call.Args = append(args[:vi], slice)
	call.IsDDD = true
}

// FixMethodCall rewrites a method call t.M(...) into a function call T.M(t, ...).
func FixMethodCall(call *ir.CallExpr) {
	if call.Fun.Op() != ir.ODOTMETH {
		return
	}

	dot := call.Fun.(*ir.SelectorExpr)

	fn := NewMethodExpr(dot.Pos(), dot.X.Type(), dot.Selection.Sym)

	args := make([]ir.Node, 1+len(call.Args))
	args[0] = dot.X
	copy(args[1:], call.Args)

	call.SetOp(ir.OCALLFUNC)
	call.Fun = fn
	call.Args = args
}

func AssertFixedCall(call *ir.CallExpr) {
	if call.Fun.Type().IsVariadic() && !call.IsDDD {
		base.FatalfAt(call.Pos(), "missed FixVariadicCall")
	}
	if call.Op() == ir.OCALLMETH {
		base.FatalfAt(call.Pos(), "missed FixMethodCall")
	}
}

// ClosureType returns the struct type used to hold all the information
// needed in the closure for clo (clo must be a OCLOSURE node).
// The address of a variable of the returned type can be cast to a func.
func ClosureType(clo *ir.ClosureExpr) *types.Type {
	// Create closure in the form of a composite literal.
	// supposing the closure captures an int i and a string s
	// and has one float64 argument and no results,
	// the generated code looks like:
	//
	//	clos = &struct{F uintptr; X0 *int; X1 *string}{func.1, &i, &s}
	//
	// The use of the struct provides type information to the garbage
	// collector so that it can walk the closure. We could use (in this
	// case) [3]unsafe.Pointer instead, but that would leave the gc in
	// the dark. The information appears in the binary in the form of
	// type descriptors; the struct is unnamed and uses exported field
	// names so that closures in multiple packages with the same struct
	// type can share the descriptor.

	fields := make([]*types.Field, 1+len(clo.Func.ClosureVars))
	fields[0] = types.NewField(base.AutogeneratedPos, types.LocalPkg.Lookup("F"), types.Types[types.TUINTPTR])
	it := NewClosureStructIter(clo.Func.ClosureVars)
	i := 0
	for {
		n, typ, _ := it.Next()
		if n == nil {
			break
		}
		fields[1+i] = types.NewField(base.AutogeneratedPos, types.LocalPkg.LookupNum("X", i), typ)
		i++
	}
	typ := types.NewStruct(fields)
	typ.SetNoalg(true)
	return typ
}

// MethodValueType returns the struct type used to hold all the information
// needed in the closure for a OMETHVALUE node. The address of a variable of
// the returned type can be cast to a func.
func MethodValueType(n *ir.SelectorExpr) *types.Type {
	t := types.NewStruct([]*types.Field{
		types.NewField(base.Pos, Lookup("F"), types.Types[types.TUINTPTR]),
		types.NewField(base.Pos, Lookup("R"), n.X.Type()),
	})
	t.SetNoalg(true)
	return t
}

// type check function definition
// To be called by typecheck, not directly.
// (Call typecheck.Func instead.)
func tcFunc(n *ir.Func) {
	if base.EnableTrace && base.Flag.LowerT {
		defer tracePrint("tcFunc", n)(nil)
	}

	if name := n.Nname; name.Typecheck() == 0 {
		base.AssertfAt(name.Type() != nil, n.Pos(), "missing type: %v", name)
		name.SetTypecheck(1)
	}
}

// tcCall typechecks an OCALL node.
func tcCall(n *ir.CallExpr, top int) ir.Node {
	Stmts(n.Init()) // imported rewritten f(g()) calls (#30907)
	n.Fun = typecheck(n.Fun, ctxExpr|ctxType|ctxCallee)

	l := n.Fun

	if l.Op() == ir.ONAME && l.(*ir.Name).BuiltinOp != 0 {
		l := l.(*ir.Name)
		if n.IsDDD && l.BuiltinOp != ir.OAPPEND {
			base.Errorf("invalid use of ... with builtin %v", l)
		}

		// builtin: OLEN, OCAP, etc.
		switch l.BuiltinOp {
		default:
			base.Fatalf("unknown builtin %v", l)

		case ir.OAPPEND, ir.ODELETE, ir.OMAKE, ir.OMAX, ir.OMIN, ir.OPRINT, ir.OPRINTLN, ir.ORECOVER:
			n.SetOp(l.BuiltinOp)
			n.Fun = nil
			n.SetTypecheck(0) // re-typechecking new op is OK, not a loop
			return typecheck(n, top)

		case ir.OCAP, ir.OCLEAR, ir.OCLOSE, ir.OIMAG, ir.OLEN, ir.OPANIC, ir.OREAL, ir.OUNSAFESTRINGDATA, ir.OUNSAFESLICEDATA:
			typecheckargs(n)
			fallthrough
		case ir.ONEW:
			arg, ok := needOneArg(n, "%v", n.Op())
			if !ok {
				n.SetType(nil)
				return n
			}
			u := ir.NewUnaryExpr(n.Pos(), l.BuiltinOp, arg)
			return typecheck(ir.InitExpr(n.Init(), u), top) // typecheckargs can add to old.Init

		case ir.OCOMPLEX, ir.OCOPY, ir.OUNSAFEADD, ir.OUNSAFESLICE, ir.OUNSAFESTRING:
			typecheckargs(n)
			arg1, arg2, ok := needTwoArgs(n)
			if !ok {
				n.SetType(nil)
				return n
			}
			b := ir.NewBinaryExpr(n.Pos(), l.BuiltinOp, arg1, arg2)
			return typecheck(ir.InitExpr(n.Init(), b), top) // typecheckargs can add to old.Init
		}
		panic("unreachable")
	}

	n.Fun = DefaultLit(n.Fun, nil)
	l = n.Fun
	if l.Op() == ir.OTYPE {
		if n.IsDDD {
			base.Fatalf("invalid use of ... in type conversion to %v", l.Type())
		}

		// pick off before type-checking arguments
		arg, ok := needOneArg(n, "conversion to %v", l.Type())
		if !ok {
			n.SetType(nil)
			return n
		}

		n := ir.NewConvExpr(n.Pos(), ir.OCONV, nil, arg)
		n.SetType(l.Type())
		return tcConv(n)
	}

	RewriteNonNameCall(n)
	typecheckargs(n)
	t := l.Type()
	if t == nil {
		n.SetType(nil)
		return n
	}
	types.CheckSize(t)

	switch l.Op() {
	case ir.ODOTINTER:
		n.SetOp(ir.OCALLINTER)

	case ir.ODOTMETH:
		l := l.(*ir.SelectorExpr)
		n.SetOp(ir.OCALLMETH)

		// typecheckaste was used here but there wasn't enough
		// information further down the call chain to know if we
		// were testing a method receiver for unexported fields.
		// It isn't necessary, so just do a sanity check.
		tp := t.Recv().Type

		if l.X == nil || !types.Identical(l.X.Type(), tp) {
			base.Fatalf("method receiver")
		}

	default:
		n.SetOp(ir.OCALLFUNC)
		if t.Kind() != types.TFUNC {
			if o := l; o.Name() != nil && types.BuiltinPkg.Lookup(o.Sym().Name).Def != nil {
				// be more specific when the non-function
				// name matches a predeclared function
				base.Errorf("cannot call non-function %L, declared at %s",
					l, base.FmtPos(o.Name().Pos()))
			} else {
				base.Errorf("cannot call non-function %L", l)
			}
			n.SetType(nil)
			return n
		}
	}

	typecheckaste(ir.OCALL, n.Fun, n.IsDDD, t.Params(), n.Args, func() string { return fmt.Sprintf("argument to %v", n.Fun) })
	FixVariadicCall(n)
	FixMethodCall(n)
	if t.NumResults() == 0 {
		return n
	}
	if t.NumResults() == 1 {
		n.SetType(l.Type().Result(0).Type)

		if n.Op() == ir.OCALLFUNC && n.Fun.Op() == ir.ONAME {
			if sym := n.Fun.(*ir.Name).Sym(); types.RuntimeSymName(sym) == "getg" {
				// Emit code for runtime.getg() directly instead of calling function.
				// Most such rewrites (for example the similar one for math.Sqrt) should be done in walk,
				// so that the ordering pass can make sure to preserve the semantics of the original code
				// (in particular, the exact time of the function call) by introducing temporaries.
				// In this case, we know getg() always returns the same result within a given function
				// and we want to avoid the temporaries, so we do the rewrite earlier than is typical.
				n.SetOp(ir.OGETG)
			}
		}
		return n
	}

	// multiple return
	if top&(ctxMultiOK|ctxStmt) == 0 {
		base.Errorf("multiple-value %v() in single-value context", l)
		return n
	}

	n.SetType(l.Type().ResultsTuple())
	return n
}

// tcAppend typechecks an OAPPEND node.
func tcAppend(n *ir.CallExpr) ir.Node {
	typecheckargs(n)
	args := n.Args
	if len(args) == 0 {
		base.Errorf("missing arguments to append")
		n.SetType(nil)
		return n
	}

	t := args[0].Type()
	if t == nil {
		n.SetType(nil)
		return n
	}

	n.SetType(t)
	if !t.IsSlice() {
		if ir.IsNil(args[0]) {
			base.Errorf("first argument to append must be typed slice; have untyped nil")
			n.SetType(nil)
			return n
		}

		base.Errorf("first argument to append must be slice; have %L", t)
		n.SetType(nil)
		return n
	}

	if n.IsDDD {
		if len(args) == 1 {
			base.Errorf("cannot use ... on first argument to append")
			n.SetType(nil)
			return n
		}

		if len(args) != 2 {
			base.Errorf("too many arguments to append")
			n.SetType(nil)
			return n
		}

		// AssignConv is of args[1] not required here, as the
		// types of args[0] and args[1] don't need to match
		// (They will both have an underlying type which are
		// slices of identical base types, or be []byte and string.)
		// See issue 53888.
		return n
	}

	as := args[1:]
	for i, n := range as {
		if n.Type() == nil {
			continue
		}
		as[i] = AssignConv(n, t.Elem(), "append")
		types.CheckSize(as[i].Type()) // ensure width is calculated for backend
	}
	return n
}

// tcClear typechecks an OCLEAR node.
func tcClear(n *ir.UnaryExpr) ir.Node {
	n.X = Expr(n.X)
	n.X = DefaultLit(n.X, nil)
	l := n.X
	t := l.Type()
	if t == nil {
		n.SetType(nil)
		return n
	}

	switch {
	case t.IsMap(), t.IsSlice():
	default:
		base.Errorf("invalid operation: %v (argument must be a map or slice)", n)
		n.SetType(nil)
		return n
	}

	return n
}

// tcClose typechecks an OCLOSE node.
func tcClose(n *ir.UnaryExpr) ir.Node {
	n.X = Expr(n.X)
	n.X = DefaultLit(n.X, nil)
	l := n.X
	t := l.Type()
	if t == nil {
		n.SetType(nil)
		return n
	}
	if !t.IsChan() {
		base.Errorf("invalid operation: %v (non-chan type %v)", n, t)
		n.SetType(nil)
		return n
	}

	if !t.ChanDir().CanSend() {
		base.Errorf("invalid operation: %v (cannot close receive-only channel)", n)
		n.SetType(nil)
		return n
	}
	return n
}

// tcComplex typechecks an OCOMPLEX node.
func tcComplex(n *ir.BinaryExpr) ir.Node {
	l := Expr(n.X)
	r := Expr(n.Y)
	if l.Type() == nil || r.Type() == nil {
		n.SetType(nil)
		return n
	}
	l, r = defaultlit2(l, r, false)
	if l.Type() == nil || r.Type() == nil {
		n.SetType(nil)
		return n
	}
	n.X = l
	n.Y = r

	if !types.Identical(l.Type(), r.Type()) {
		base.Errorf("invalid operation: %v (mismatched types %v and %v)", n, l.Type(), r.Type())
		n.SetType(nil)
		return n
	}

	var t *types.Type
	switch l.Type().Kind() {
	default:
		base.Errorf("invalid operation: %v (arguments have type %v, expected floating-point)", n, l.Type())
		n.SetType(nil)
		return n

	case types.TIDEAL:
		t = types.UntypedComplex

	case types.TFLOAT32:
		t = types.Types[types.TCOMPLEX64]

	case types.TFLOAT64:
		t = types.Types[types.TCOMPLEX128]
	}
	n.SetType(t)
	return n
}

// tcCopy typechecks an OCOPY node.
func tcCopy(n *ir.BinaryExpr) ir.Node {
	n.SetType(types.Types[types.TINT])
	n.X = Expr(n.X)
	n.X = DefaultLit(n.X, nil)
	n.Y = Expr(n.Y)
	n.Y = DefaultLit(n.Y, nil)
	if n.X.Type() == nil || n.Y.Type() == nil {
		n.SetType(nil)
		return n
	}

	// copy([]byte, string)
	if n.X.Type().IsSlice() && n.Y.Type().IsString() {
		if types.Identical(n.X.Type().Elem(), types.ByteType) {
			return n
		}
		base.Errorf("arguments to copy have different element types: %L and string", n.X.Type())
		n.SetType(nil)
		return n
	}

	if !n.X.Type().IsSlice() || !n.Y.Type().IsSlice() {
		if !n.X.Type().IsSlice() && !n.Y.Type().IsSlice() {
			base.Errorf("arguments to copy must be slices; have %L, %L", n.X.Type(), n.Y.Type())
		} else if !n.X.Type().IsSlice() {
			base.Errorf("first argument to copy should be slice; have %L", n.X.Type())
		} else {
			base.Errorf("second argument to copy should be slice or string; have %L", n.Y.Type())
		}
		n.SetType(nil)
		return n
	}

	if !types.Identical(n.X.Type().Elem(), n.Y.Type().Elem()) {
		base.Errorf("arguments to copy have different element types: %L and %L", n.X.Type(), n.Y.Type())
		n.SetType(nil)
		return n
	}
	return n
}

// tcDelete typechecks an ODELETE node.
func tcDelete(n *ir.CallExpr) ir.Node {
	typecheckargs(n)
	args := n.Args
	if len(args) == 0 {
		base.Errorf("missing arguments to delete")
		n.SetType(nil)
		return n
	}

	if len(args) == 1 {
		base.Errorf("missing second (key) argument to delete")
		n.SetType(nil)
		return n
	}

	if len(args) != 2 {
		base.Errorf("too many arguments to delete")
		n.SetType(nil)
		return n
	}

	l := args[0]
	r := args[1]
	if l.Type() != nil && !l.Type().IsMap() {
		base.Errorf("first argument to delete must be map; have %L", l.Type())
		n.SetType(nil)
		return n
	}

	args[1] = AssignConv(r, l.Type().Key(), "delete")
	return n
}

// tcMake typechecks an OMAKE node.
func tcMake(n *ir.CallExpr) ir.Node {
	args := n.Args
	if len(args) == 0 {
		base.Errorf("missing argument to make")
		n.SetType(nil)
		return n
	}

	n.Args = nil
	l := args[0]
	l = typecheck(l, ctxType)
	t := l.Type()
	if t == nil {
		n.SetType(nil)
		return n
	}

	i := 1
	var nn ir.Node
	switch t.Kind() {
	default:
		base.Errorf("cannot make type %v", t)
		n.SetType(nil)
		return n

	case types.TSLICE:
		if i >= len(args) {
			base.Errorf("missing len argument to make(%v)", t)
			n.SetType(nil)
			return n
		}

		l = args[i]
		i++
		l = Expr(l)
		var r ir.Node
		if i < len(args) {
			r = args[i]
			i++
			r = Expr(r)
		}

		if l.Type() == nil || (r != nil && r.Type() == nil) {
			n.SetType(nil)
			return n
		}
		if !checkmake(t, "len", &l) || r != nil && !checkmake(t, "cap", &r) {
			n.SetType(nil)
			return n
		}
		if ir.IsConst(l, constant.Int) && r != nil && ir.IsConst(r, constant.Int) && constant.Compare(l.Val(), token.GTR, r.Val()) {
			base.Errorf("len larger than cap in make(%v)", t)
			n.SetType(nil)
			return n
		}
		nn = ir.NewMakeExpr(n.Pos(), ir.OMAKESLICE, l, r)

	case types.TMAP:
		if i < len(args) {
			l = args[i]
			i++
			l = Expr(l)
			l = DefaultLit(l, types.Types[types.TINT])
			if l.Type() == nil {
				n.SetType(nil)
				return n
			}
			if !checkmake(t, "size", &l) {
				n.SetType(nil)
				return n
			}
		} else {
			l = ir.NewInt(base.Pos, 0)
		}
		nn = ir.NewMakeExpr(n.Pos(), ir.OMAKEMAP, l, nil)
		nn.SetEsc(n.Esc())

	case types.TCHAN:
		l = nil
		if i < len(args) {
			l = args[i]
			i++
			l = Expr(l)
			l = DefaultLit(l, types.Types[types.TINT])
			if l.Type() == nil {
				n.SetType(nil)
				return n
			}
			if !checkmake(t, "buffer", &l) {
				n.SetType(nil)
				return n
			}
		} else {
			l = ir.NewInt(base.Pos, 0)
		}
		nn = ir.NewMakeExpr(n.Pos(), ir.OMAKECHAN, l, nil)
	}

	if i < len(args) {
		base.Errorf("too many arguments to make(%v)", t)
		n.SetType(nil)
		return n
	}

	nn.SetType(t)
	return nn
}

// tcMakeSliceCopy typechecks an OMAKESLICECOPY node.
func tcMakeSliceCopy(n *ir.MakeExpr) ir.Node {
	// Errors here are Fatalf instead of Errorf because only the compiler
	// can construct an OMAKESLICECOPY node.
	// Components used in OMAKESCLICECOPY that are supplied by parsed source code
	// have already been typechecked in OMAKE and OCOPY earlier.
	t := n.Type()

	if t == nil {
		base.Fatalf("no type specified for OMAKESLICECOPY")
	}

	if !t.IsSlice() {
		base.Fatalf("invalid type %v for OMAKESLICECOPY", n.Type())
	}

	if n.Len == nil {
		base.Fatalf("missing len argument for OMAKESLICECOPY")
	}

	if n.Cap == nil {
		base.Fatalf("missing slice argument to copy for OMAKESLICECOPY")
	}

	n.Len = Expr(n.Len)
	n.Cap = Expr(n.Cap)

	n.Len = DefaultLit(n.Len, types.Types[types.TINT])

	if !n.Len.Type().IsInteger() && n.Type().Kind() != types.TIDEAL {
		base.Errorf("non-integer len argument in OMAKESLICECOPY")
	}

	if ir.IsConst(n.Len, constant.Int) {
		if ir.ConstOverflow(n.Len.Val(), types.Types[types.TINT]) {
			base.Fatalf("len for OMAKESLICECOPY too large")
		}
		if constant.Sign(n.Len.Val()) < 0 {
			base.Fatalf("len for OMAKESLICECOPY must be non-negative")
		}
	}
	return n
}

// tcNew typechecks an ONEW node.
func tcNew(n *ir.UnaryExpr) ir.Node {
	if n.X == nil {
		// Fatalf because the OCALL above checked for us,
		// so this must be an internally-generated mistake.
		base.Fatalf("missing argument to new")
	}
	l := n.X
	l = typecheck(l, ctxType)
	t := l.Type()
	if t == nil {
		n.SetType(nil)
		return n
	}
	n.X = l
	n.SetType(types.NewPtr(t))
	return n
}

// tcPanic typechecks an OPANIC node.
func tcPanic(n *ir.UnaryExpr) ir.Node {
	n.X = Expr(n.X)
	n.X = AssignConv(n.X, types.Types[types.TINTER], "argument to panic")
	if n.X.Type() == nil {
		n.SetType(nil)
		return n
	}
	return n
}

// tcPrint typechecks an OPRINT or OPRINTN node.
func tcPrint(n *ir.CallExpr) ir.Node {
	typecheckargs(n)
	ls := n.Args
	for i1, n1 := range ls {
		// Special case for print: int constant is int64, not int.
		if ir.IsConst(n1, constant.Int) {
			ls[i1] = DefaultLit(ls[i1], types.Types[types.TINT64])
		} else {
			ls[i1] = DefaultLit(ls[i1], nil)
		}
	}
	return n
}

// tcMinMax typechecks an OMIN or OMAX node.
func tcMinMax(n *ir.CallExpr) ir.Node {
	typecheckargs(n)
	arg0 := n.Args[0]
	for _, arg := range n.Args[1:] {
		if !types.Identical(arg.Type(), arg0.Type()) {
			base.FatalfAt(n.Pos(), "mismatched arguments: %L and %L", arg0, arg)
		}
	}
	n.SetType(arg0.Type())
	return n
}

// tcRealImag typechecks an OREAL or OIMAG node.
func tcRealImag(n *ir.UnaryExpr) ir.Node {
	n.X = Expr(n.X)
	l := n.X
	t := l.Type()
	if t == nil {
		n.SetType(nil)
		return n
	}

	// Determine result type.
	switch t.Kind() {
	case types.TIDEAL:
		n.SetType(types.UntypedFloat)
	case types.TCOMPLEX64:
		n.SetType(types.Types[types.TFLOAT32])
	case types.TCOMPLEX128:
		n.SetType(types.Types[types.TFLOAT64])
	default:
		base.Errorf("invalid argument %L for %v", l, n.Op())
		n.SetType(nil)
		return n
	}
	return n
}

// tcRecover typechecks an ORECOVER node.
func tcRecover(n *ir.CallExpr) ir.Node {
	if len(n.Args) != 0 {
		base.Errorf("too many arguments to recover")
		n.SetType(nil)
		return n
	}

	n.SetType(types.Types[types.TINTER])
	return n
}

// tcUnsafeAdd typechecks an OUNSAFEADD node.
func tcUnsafeAdd(n *ir.BinaryExpr) *ir.BinaryExpr {
	n.X = AssignConv(Expr(n.X), types.Types[types.TUNSAFEPTR], "argument to unsafe.Add")
	n.Y = DefaultLit(Expr(n.Y), types.Types[types.TINT])
	if n.X.Type() == nil || n.Y.Type() == nil {
		n.SetType(nil)
		return n
	}
	if !n.Y.Type().IsInteger() {
		n.SetType(nil)
		return n
	}
	n.SetType(n.X.Type())
	return n
}

// tcUnsafeSlice typechecks an OUNSAFESLICE node.
func tcUnsafeSlice(n *ir.BinaryExpr) *ir.BinaryExpr {
	n.X = Expr(n.X)
	n.Y = Expr(n.Y)
	if n.X.Type() == nil || n.Y.Type() == nil {
		n.SetType(nil)
		return n
	}
	t := n.X.Type()
	if !t.IsPtr() {
		base.Errorf("first argument to unsafe.Slice must be pointer; have %L", t)
	} else if t.Elem().NotInHeap() {
		// TODO(mdempsky): This can be relaxed, but should only affect the
		// Go runtime itself. End users should only see not-in-heap
		// types due to incomplete C structs in cgo, and those types don't
		// have a meaningful size anyway.
		base.Errorf("unsafe.Slice of incomplete (or unallocatable) type not allowed")
	}

	if !checkunsafesliceorstring(n.Op(), &n.Y) {
		n.SetType(nil)
		return n
	}
	n.SetType(types.NewSlice(t.Elem()))
	return n
}

// tcUnsafeString typechecks an OUNSAFESTRING node.
func tcUnsafeString(n *ir.BinaryExpr) *ir.BinaryExpr {
	n.X = Expr(n.X)
	n.Y = Expr(n.Y)
	if n.X.Type() == nil || n.Y.Type() == nil {
		n.SetType(nil)
		return n
	}
	t := n.X.Type()
	if !t.IsPtr() || !types.Identical(t.Elem(), types.Types[types.TUINT8]) {
		base.Errorf("first argument to unsafe.String must be *byte; have %L", t)
	}

	if !checkunsafesliceorstring(n.Op(), &n.Y) {
		n.SetType(nil)
		return n
	}
	n.SetType(types.Types[types.TSTRING])
	return n
}

// ClosureStructIter iterates through a slice of closure variables returning
// their type and offset in the closure struct.
type ClosureStructIter struct {
	closureVars []*ir.Name
	offset      int64
	next        int
}

// NewClosureStructIter creates a new ClosureStructIter for closureVars.
func NewClosureStructIter(closureVars []*ir.Name) *ClosureStructIter {
	return &ClosureStructIter{
		closureVars: closureVars,
		offset:      int64(types.PtrSize), // PtrSize to skip past function entry PC field
		next:        0,
	}
}

// Next returns the next name, type and offset of the next closure variable.
// A nil name is returned after the last closure variable.
func (iter *ClosureStructIter) Next() (n *ir.Name, typ *types.Type, offset int64) {
	if iter.next >= len(iter.closureVars) {
		return nil, nil, 0
	}
	n = iter.closureVars[iter.next]
	typ = n.Type()
	if !n.Byval() {
		typ = types.NewPtr(typ)
	}
	iter.next++
	offset = types.RoundUp(iter.offset, typ.Alignment())
	iter.offset = offset + typ.Size()
	return n, typ, offset
}
