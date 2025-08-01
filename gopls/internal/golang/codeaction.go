// Copyright 2024 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package golang

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"reflect"
	"slices"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/ast/edge"
	"golang.org/x/tools/go/ast/inspector"
	"golang.org/x/tools/gopls/internal/analysis/fillstruct"
	"golang.org/x/tools/gopls/internal/analysis/fillswitch"
	"golang.org/x/tools/gopls/internal/cache"
	"golang.org/x/tools/gopls/internal/cache/parsego"
	"golang.org/x/tools/gopls/internal/file"
	"golang.org/x/tools/gopls/internal/golang/stubmethods"
	"golang.org/x/tools/gopls/internal/label"
	"golang.org/x/tools/gopls/internal/protocol"
	"golang.org/x/tools/gopls/internal/protocol/command"
	"golang.org/x/tools/gopls/internal/settings"
	"golang.org/x/tools/internal/event"
	"golang.org/x/tools/internal/imports"
	"golang.org/x/tools/internal/typesinternal"
)

// CodeActions returns all enabled code actions (edits and other
// commands) available for the selected range.
//
// Depending on how the request was triggered, fewer actions may be
// offered, e.g. to avoid UI distractions after mere cursor motion.
//
// See ../protocol/codeactionkind.go for some code action theory.
func CodeActions(ctx context.Context, snapshot *cache.Snapshot, fh file.Handle, rng protocol.Range, diagnostics []protocol.Diagnostic, enabled func(protocol.CodeActionKind) bool, trigger protocol.CodeActionTriggerKind) (actions []protocol.CodeAction, _ error) {
	loc := fh.URI().Location(rng)

	pgf, err := snapshot.ParseGo(ctx, fh, parsego.Full)
	if err != nil {
		return nil, err
	}
	start, end, err := pgf.RangePos(rng)
	if err != nil {
		return nil, err
	}

	// Scan to see if any enabled producer needs type information.
	var enabledMemo [len(codeActionProducers)]bool
	needTypes := false
	for i, p := range codeActionProducers {
		if enabled(p.kind) {
			enabledMemo[i] = true
			if p.needPkg {
				needTypes = true
			}
		}
	}

	// Compute type information if needed.
	// Also update pgf, start, end to be consistent with pkg.
	// They may differ in case of parse cache miss.
	var pkg *cache.Package
	if needTypes {
		var err error
		pkg, pgf, err = NarrowestPackageForFile(ctx, snapshot, loc.URI)
		if err != nil {
			return nil, err
		}
		start, end, err = pgf.RangePos(loc.Range)
		if err != nil {
			return nil, err
		}
	}

	// Execute each enabled producer function.
	req := &codeActionsRequest{
		actions:     &actions,
		lazy:        make(map[reflect.Type]any),
		snapshot:    snapshot,
		fh:          fh,
		pgf:         pgf,
		loc:         loc,
		start:       start,
		end:         end,
		diagnostics: diagnostics,
		trigger:     trigger,
		pkg:         pkg,
	}
	for i, p := range codeActionProducers {
		if !enabledMemo[i] {
			continue
		}
		req.kind = p.kind
		if p.needPkg {
			req.pkg = pkg
		} else {
			req.pkg = nil
		}
		if err := p.fn(ctx, req); err != nil {
			// An error in one code action producer
			// should not affect the others.
			if ctx.Err() != nil {
				return nil, err
			}
			event.Error(ctx, fmt.Sprintf("CodeAction producer %s failed", p.kind), err)
			continue
		}
	}

	// Return code actions in the order their providers are listed.
	return actions, nil
}

// A codeActionsRequest is passed to each function
// that produces code actions.
type codeActionsRequest struct {
	// internal fields for use only by [CodeActions].
	actions *[]protocol.CodeAction // pointer to output slice; call addAction to populate
	lazy    map[reflect.Type]any   // lazy construction

	// inputs to the producer function:
	kind        protocol.CodeActionKind
	snapshot    *cache.Snapshot
	fh          file.Handle
	pgf         *parsego.File
	loc         protocol.Location
	start, end  token.Pos
	diagnostics []protocol.Diagnostic
	trigger     protocol.CodeActionTriggerKind
	pkg         *cache.Package // set only if producer.needPkg
}

// addApplyFixAction adds an ApplyFix command-based CodeAction to the result.
func (req *codeActionsRequest) addApplyFixAction(title, fix string, loc protocol.Location) {
	cmd := command.NewApplyFixCommand(title, command.ApplyFixArgs{
		Fix:          fix,
		Location:     loc,
		ResolveEdits: req.resolveEdits(),
	})
	req.addCommandAction(cmd, true)
}

// addCommandAction adds a CodeAction to the result based on the provided command.
//
// If allowResolveEdits (and the client supports codeAction/resolve)
// then the command is embedded into the code action data field so
// that the client can later ask the server to "resolve" a command
// into an edit that they can preview and apply selectively.
// IMPORTANT: set allowResolveEdits only for actions that are 'edit aware',
// meaning they can detect when they are being executed in the context of a
// codeAction/resolve request, and return edits rather than applying them using
// workspace/applyEdit. In golang/go#71405, edits were being apply during the
// codeAction/resolve request handler.
// TODO(rfindley): refactor the command and code lens registration APIs so that
// resolve edit support is inferred from the command signature, not dependent
// on coordination between codeAction and command logic.
//
// Otherwise, the command is set as the code action operation.
func (req *codeActionsRequest) addCommandAction(cmd *protocol.Command, allowResolveEdits bool) {
	act := protocol.CodeAction{
		Title: cmd.Title,
		Kind:  req.kind,
	}
	if allowResolveEdits && req.resolveEdits() {
		data, err := json.Marshal(cmd)
		if err != nil {
			panic("unable to marshal")
		}
		msg := json.RawMessage(data)
		act.Data = &msg
	} else {
		act.Command = cmd
	}
	req.addAction(act)
}

// addEditAction adds an edit-based CodeAction to the result.
func (req *codeActionsRequest) addEditAction(title string, fixedDiagnostics []protocol.Diagnostic, changes ...protocol.DocumentChange) {
	req.addAction(protocol.CodeAction{
		Title:       title,
		Kind:        req.kind,
		Diagnostics: fixedDiagnostics,
		Edit:        protocol.NewWorkspaceEdit(changes...),
	})
}

// addAction adds a code action to the response.
func (req *codeActionsRequest) addAction(act protocol.CodeAction) {
	*req.actions = append(*req.actions, act)
}

// resolveEdits reports whether the client can resolve edits lazily.
func (req *codeActionsRequest) resolveEdits() bool {
	opts := req.snapshot.Options()
	return opts.CodeActionResolveOptions != nil &&
		slices.Contains(opts.CodeActionResolveOptions, "edit")
}

// lazyInit[*T](ctx, req) returns a pointer to an instance of T,
// calling new(T).init(ctx.req) on the first request.
//
// It is conceptually a (generic) method of req.
func lazyInit[P interface {
	init(ctx context.Context, req *codeActionsRequest)
	*T
}, T any](ctx context.Context, req *codeActionsRequest) P {
	t := reflect.TypeFor[T]()
	v, ok := req.lazy[t].(P)
	if !ok {
		v = new(T)
		v.init(ctx, req)
		req.lazy[t] = v
	}
	return v
}

// -- producers --

// A codeActionProducer describes a function that produces CodeActions
// of a particular kind.
// The function is only called if that kind is enabled.
type codeActionProducer struct {
	kind    protocol.CodeActionKind
	fn      func(ctx context.Context, req *codeActionsRequest) error
	needPkg bool // fn needs type information (req.pkg)
}

// Code Actions are returned in the order their producers are listed below.
// Depending on the client, this may influence the order they appear in the UI.
var codeActionProducers = [...]codeActionProducer{
	{kind: protocol.QuickFix, fn: quickFix, needPkg: true},
	{kind: protocol.SourceOrganizeImports, fn: sourceOrganizeImports},
	{kind: settings.AddTest, fn: addTest, needPkg: true},
	{kind: settings.GoAssembly, fn: goAssembly, needPkg: true},
	{kind: settings.GoDoc, fn: goDoc, needPkg: true},
	{kind: settings.GoFreeSymbols, fn: goFreeSymbols},
	{kind: settings.GoSplitPackage, fn: goSplitPackage, needPkg: true},
	{kind: settings.GoTest, fn: goTest, needPkg: true},
	{kind: settings.GoToggleCompilerOptDetails, fn: toggleCompilerOptDetails},
	{kind: settings.RefactorExtractFunction, fn: refactorExtractFunction},
	{kind: settings.RefactorExtractMethod, fn: refactorExtractMethod},
	{kind: settings.RefactorExtractToNewFile, fn: refactorExtractToNewFile},
	{kind: settings.RefactorExtractConstant, fn: refactorExtractVariable, needPkg: true},
	{kind: settings.RefactorExtractVariable, fn: refactorExtractVariable, needPkg: true},
	{kind: settings.RefactorExtractConstantAll, fn: refactorExtractVariableAll, needPkg: true},
	{kind: settings.RefactorExtractVariableAll, fn: refactorExtractVariableAll, needPkg: true},
	{kind: settings.RefactorInlineCall, fn: refactorInlineCall, needPkg: true},
	{kind: settings.RefactorInlineVariable, fn: refactorInlineVariable, needPkg: true},
	{kind: settings.RefactorRewriteChangeQuote, fn: refactorRewriteChangeQuote},
	{kind: settings.RefactorRewriteFillStruct, fn: refactorRewriteFillStruct, needPkg: true},
	{kind: settings.RefactorRewriteFillSwitch, fn: refactorRewriteFillSwitch, needPkg: true},
	{kind: settings.RefactorRewriteInvertIf, fn: refactorRewriteInvertIf},
	{kind: settings.RefactorRewriteJoinLines, fn: refactorRewriteJoinLines, needPkg: true},
	{kind: settings.RefactorRewriteRemoveUnusedParam, fn: refactorRewriteRemoveUnusedParam, needPkg: true},
	{kind: settings.RefactorRewriteMoveParamLeft, fn: refactorRewriteMoveParamLeft, needPkg: true},
	{kind: settings.RefactorRewriteMoveParamRight, fn: refactorRewriteMoveParamRight, needPkg: true},
	{kind: settings.RefactorRewriteSplitLines, fn: refactorRewriteSplitLines, needPkg: true},
	{kind: settings.RefactorRewriteEliminateDotImport, fn: refactorRewriteEliminateDotImport, needPkg: true},
	{kind: settings.RefactorRewriteAddTags, fn: refactorRewriteAddStructTags, needPkg: true},
	{kind: settings.RefactorRewriteRemoveTags, fn: refactorRewriteRemoveStructTags, needPkg: true},
	{kind: settings.GoplsDocFeatures, fn: goplsDocFeatures}, // offer this one last (#72742)

	// Note: don't forget to update the allow-list in Server.CodeAction
	// when adding new query operations like GoTest and GoDoc that
	// are permitted even in generated source files.
}

// sourceOrganizeImports produces "Organize Imports" code actions.
func sourceOrganizeImports(ctx context.Context, req *codeActionsRequest) error {
	res := lazyInit[*allImportsFixesResult](ctx, req)

	// Send all of the import edits as one code action
	// if the file is being organized.
	if len(res.allFixEdits) > 0 {
		req.addEditAction("Organize Imports", nil, protocol.DocumentChangeEdit(req.fh, res.allFixEdits))
	}

	return nil
}

// quickFix produces code actions that fix errors,
// for example by adding/deleting/renaming imports,
// or declaring the missing methods of a type.
func quickFix(ctx context.Context, req *codeActionsRequest) error {
	// Only compute quick fixes if there are any diagnostics to fix.
	if len(req.diagnostics) == 0 {
		return nil
	}

	// Process any missing imports and pair them with the diagnostics they fix.
	res := lazyInit[*allImportsFixesResult](ctx, req)
	if res.err != nil {
		return nil
	}

	// Separate this into a set of codeActions per diagnostic, where
	// each action is the addition, removal, or renaming of one import.
	for _, importFix := range res.editsPerFix {
		fixedDiags := fixedByImportFix(importFix.fix, req.diagnostics)
		if len(fixedDiags) == 0 {
			continue
		}
		req.addEditAction(importFixTitle(importFix.fix), fixedDiags, protocol.DocumentChangeEdit(req.fh, importFix.edits))
	}

	// Quick fixes for type errors.
	info := req.pkg.TypesInfo()
	for _, typeError := range req.pkg.TypeErrors() {
		// Does type error overlap with CodeAction range?
		start, end := typeError.Pos, typeError.Pos
		if _, _, endPos, ok := typesinternal.ErrorCodeStartEnd(typeError); ok {
			end = endPos
		}
		typeErrorRange, err := req.pgf.PosRange(start, end)
		if err != nil || !protocol.Intersect(typeErrorRange, req.loc.Range) {
			continue
		}

		msg := typeError.Msg
		switch {
		// "Missing method" error? (stubmethods)
		// Offer a "Declare missing methods of INTERFACE" code action.
		// See [stubMissingInterfaceMethodsFixer] for command implementation.
		case strings.Contains(msg, "missing method"),
			strings.HasPrefix(msg, "cannot convert"),
			strings.Contains(msg, "not implement"):
			si := stubmethods.GetIfaceStubInfo(req.pkg.FileSet(), info, req.pgf, start, end)
			if si != nil {
				qual := typesinternal.FileQualifier(req.pgf.File, si.Concrete.Obj().Pkg())
				iface := types.TypeString(si.Interface.Type(), qual)
				msg := fmt.Sprintf("Declare missing methods of %s", iface)
				req.addApplyFixAction(msg, fixMissingInterfaceMethods, req.loc)
			}

		// "type X has no field or method Y" compiler error.
		// Offer a "Declare missing method T.f" code action.
		// See [stubMissingCalledFunctionFixer] for command implementation.
		case strings.Contains(msg, "has no field or method"):
			si := stubmethods.GetCallStubInfo(req.pkg.FileSet(), info, req.pgf, start, end)
			if si != nil {
				msg := fmt.Sprintf("Declare missing method %s.%s", si.Receiver.Obj().Name(), si.MethodName)
				req.addApplyFixAction(msg, fixMissingCalledFunction, req.loc)
			}

		// "undeclared name: X" or "undefined: X" compiler error.
		// Offer a "Create variable/function X" code action.
		// See [createUndeclared] for command implementation.
		case strings.HasPrefix(msg, "undeclared name: "),
			strings.HasPrefix(msg, "undefined: "):
			path, _ := astutil.PathEnclosingInterval(req.pgf.File, start, end)
			title := undeclaredFixTitle(path, msg)
			if title != "" {
				req.addApplyFixAction(title, fixCreateUndeclared, req.loc)
			}
		}
	}

	return nil
}

// allImportsFixesResult is the result of a lazy call to allImportsFixes.
// It implements the codeActionsRequest lazyInit interface.
type allImportsFixesResult struct {
	allFixEdits []protocol.TextEdit
	editsPerFix []*importFix
	err         error
}

func (res *allImportsFixesResult) init(ctx context.Context, req *codeActionsRequest) {
	res.allFixEdits, res.editsPerFix, res.err = allImportsFixes(ctx, req.snapshot, req.pgf)
	if res.err != nil {
		event.Error(ctx, "imports fixes", res.err, label.File.Of(req.loc.URI.Path()))
	}
}

func importFixTitle(fix *imports.ImportFix) string {
	var str string
	switch fix.FixType {
	case imports.AddImport:
		str = fmt.Sprintf("Add import: %s %q", fix.StmtInfo.Name, fix.StmtInfo.ImportPath)
	case imports.DeleteImport:
		str = fmt.Sprintf("Delete import: %s %q", fix.StmtInfo.Name, fix.StmtInfo.ImportPath)
	case imports.SetImportName:
		str = fmt.Sprintf("Rename import: %s %q", fix.StmtInfo.Name, fix.StmtInfo.ImportPath)
	}
	return str
}

// fixedByImportFix filters the provided slice of diagnostics to those that
// would be fixed by the provided imports fix.
func fixedByImportFix(fix *imports.ImportFix, diagnostics []protocol.Diagnostic) []protocol.Diagnostic {
	var results []protocol.Diagnostic
	for _, diagnostic := range diagnostics {
		switch {
		// "undeclared name: X" may be an unresolved import.
		case strings.HasPrefix(diagnostic.Message, "undeclared name: "):
			ident := strings.TrimPrefix(diagnostic.Message, "undeclared name: ")
			if ident == fix.IdentName {
				results = append(results, diagnostic)
			}
		// "undefined: X" may be an unresolved import at Go 1.20+.
		case strings.HasPrefix(diagnostic.Message, "undefined: "):
			ident := strings.TrimPrefix(diagnostic.Message, "undefined: ")
			if ident == fix.IdentName {
				results = append(results, diagnostic)
			}
		// "could not import: X" may be an invalid import.
		case strings.HasPrefix(diagnostic.Message, "could not import: "):
			ident := strings.TrimPrefix(diagnostic.Message, "could not import: ")
			if ident == fix.IdentName {
				results = append(results, diagnostic)
			}
		// "X imported but not used" is an unused import.
		// "X imported but not used as Y" is an unused import.
		case strings.Contains(diagnostic.Message, " imported but not used"):
			idx := strings.Index(diagnostic.Message, " imported but not used")
			importPath := diagnostic.Message[:idx]
			if importPath == fmt.Sprintf("%q", fix.StmtInfo.ImportPath) {
				results = append(results, diagnostic)
			}
		}
	}
	return results
}

// goFreeSymbols produces "Browse free symbols" code actions.
// See [server.commandHandler.FreeSymbols] for command implementation.
func goFreeSymbols(ctx context.Context, req *codeActionsRequest) error {
	if !req.loc.Empty() {
		cmd := command.NewFreeSymbolsCommand("Browse free symbols", req.snapshot.View().ID(), req.loc)
		req.addCommandAction(cmd, false)
	}
	return nil
}

// goSplitPackage produces "Split package p" code actions.
// See [server.commandHandler.SplitPackage] for command implementation.
func goSplitPackage(ctx context.Context, req *codeActionsRequest) error {
	// TODO(adonovan): ideally we would key by the package path,
	// or the ID of the widest package for the current file,
	// so that we don't see different results when toggling
	// between p.go and p_test.go.
	//
	// TODO(adonovan): opt: req should always provide metadata so
	// that we don't have to request type checking (needPkg=true).
	meta := req.pkg.Metadata()
	title := fmt.Sprintf("Split package %q", meta.Name)
	cmd := command.NewSplitPackageCommand(title, req.snapshot.View().ID(), string(meta.ID))
	req.addCommandAction(cmd, false)
	return nil
}

// goplsDocFeatures produces "Browse gopls feature documentation" code actions.
// See [server.commandHandler.ClientOpenURL] for command implementation.
func goplsDocFeatures(ctx context.Context, req *codeActionsRequest) error {
	cmd := command.NewClientOpenURLCommand(
		"Browse gopls feature documentation",
		"https://go.dev/gopls/features")
	req.addCommandAction(cmd, false)
	return nil
}

// goDoc produces "Browse documentation for X" code actions.
// See [server.commandHandler.Doc] for command implementation.
func goDoc(ctx context.Context, req *codeActionsRequest) error {
	_, _, title := DocFragment(req.pkg, req.pgf, req.start, req.end)
	if title != "" {
		cmd := command.NewDocCommand(title, command.DocArgs{Location: req.loc, ShowDocument: true})
		req.addCommandAction(cmd, false)
	}
	return nil
}

// refactorExtractFunction produces "Extract function" code actions.
// See [extractFunction] for command implementation.
func refactorExtractFunction(ctx context.Context, req *codeActionsRequest) error {
	if _, ok, _, _ := canExtractFunction(req.pgf.Tok, req.start, req.end, req.pgf.Src, req.pgf.Cursor); ok {
		req.addApplyFixAction("Extract function", fixExtractFunction, req.loc)
	}
	return nil
}

// refactorExtractMethod produces "Extract method" code actions.
// See [extractMethod] for command implementation.
func refactorExtractMethod(ctx context.Context, req *codeActionsRequest) error {
	if _, ok, methodOK, _ := canExtractFunction(req.pgf.Tok, req.start, req.end, req.pgf.Src, req.pgf.Cursor); ok && methodOK {
		req.addApplyFixAction("Extract method", fixExtractMethod, req.loc)
	}
	return nil
}

// refactorExtractVariable produces "Extract variable|constant" code actions.
// See [extractVariable] for command implementation.
func refactorExtractVariable(ctx context.Context, req *codeActionsRequest) error {
	info := req.pkg.TypesInfo()
	if exprs, err := canExtractVariable(info, req.pgf.Cursor, req.start, req.end, false); err == nil {
		// Offer one of refactor.extract.{constant,variable}
		// based on the constness of the expression; this is a
		// limitation of the codeActionProducers mechanism.
		// Beware that future evolutions of the refactorings
		// may make them diverge to become non-complementary,
		// for example because "if const x = ...; y {" is illegal.
		// Same as [refactorExtractVariableAll].
		constant := info.Types[exprs[0]].Value != nil
		if (req.kind == settings.RefactorExtractConstant) == constant {
			title := "Extract variable"
			if constant {
				title = "Extract constant"
			}
			req.addApplyFixAction(title, fixExtractVariable, req.loc)
		}
	}
	return nil
}

// refactorExtractVariableAll produces "Extract N occurrences of EXPR" code action.
// See [extractVariable] for implementation.
func refactorExtractVariableAll(ctx context.Context, req *codeActionsRequest) error {
	info := req.pkg.TypesInfo()
	// Don't suggest if only one expr is found,
	// otherwise it will duplicate with [refactorExtractVariable]
	if exprs, err := canExtractVariable(info, req.pgf.Cursor, req.start, req.end, true); err == nil && len(exprs) > 1 {
		text, err := req.pgf.NodeText(exprs[0])
		if err != nil {
			return err
		}
		desc := string(text)
		if len(desc) >= 40 || strings.Contains(desc, "\n") {
			desc = astutil.NodeDescription(exprs[0])
		}
		constant := info.Types[exprs[0]].Value != nil
		if (req.kind == settings.RefactorExtractConstantAll) == constant {
			var title string
			if constant {
				title = fmt.Sprintf("Extract %d occurrences of const expression: %s", len(exprs), desc)
			} else {
				title = fmt.Sprintf("Extract %d occurrences of %s", len(exprs), desc)
			}
			req.addApplyFixAction(title, fixExtractVariableAll, req.loc)
		}
	}
	return nil
}

// refactorExtractToNewFile produces "Extract declarations to new file" code actions.
// See [server.commandHandler.ExtractToNewFile] for command implementation.
func refactorExtractToNewFile(ctx context.Context, req *codeActionsRequest) error {
	if canExtractToNewFile(req.pgf, req.start, req.end) {
		cmd := command.NewExtractToNewFileCommand("Extract declarations to new file", req.loc)
		req.addCommandAction(cmd, false)
	}
	return nil
}

// addTest produces "Add test for FUNC" code actions.
// See [server.commandHandler.AddTest] for command implementation.
func addTest(ctx context.Context, req *codeActionsRequest) error {
	// Reject test package.
	if req.pkg.Metadata().ForTest != "" {
		return nil
	}

	path, _ := astutil.PathEnclosingInterval(req.pgf.File, req.start, req.end)
	if len(path) < 2 {
		return nil
	}

	decl, ok := path[len(path)-2].(*ast.FuncDecl)
	if !ok {
		return nil
	}

	// Don't offer to create tests of "init" or "_".
	if decl.Name.Name == "_" || decl.Name.Name == "init" {
		return nil
	}

	// TODO(hxjiang): support functions with type parameter.
	if decl.Type.TypeParams != nil {
		return nil
	}

	cmd := command.NewAddTestCommand("Add test for "+decl.Name.String(), req.loc)
	req.addCommandAction(cmd, false)

	// TODO(hxjiang): add code action for generate test for package/file.
	return nil
}

// identityTransform returns a change signature transformation that leaves the
// given fieldlist unmodified.
func identityTransform(fields *ast.FieldList) []command.ChangeSignatureParam {
	var id []command.ChangeSignatureParam
	for i := 0; i < fields.NumFields(); i++ {
		id = append(id, command.ChangeSignatureParam{OldIndex: i})
	}
	return id
}

// refactorRewriteRemoveUnusedParam produces "Remove unused parameter" code actions.
// See [server.commandHandler.ChangeSignature] for command implementation.
func refactorRewriteRemoveUnusedParam(ctx context.Context, req *codeActionsRequest) error {
	if info := removableParameter(req.pkg, req.pgf, req.loc.Range); info != nil {
		var transform []command.ChangeSignatureParam
		for i := 0; i < info.decl.Type.Params.NumFields(); i++ {
			if i != info.paramIndex {
				transform = append(transform, command.ChangeSignatureParam{OldIndex: i})
			}
		}
		cmd := command.NewChangeSignatureCommand("Remove unused parameter", command.ChangeSignatureArgs{
			Location:     req.loc,
			NewParams:    transform,
			NewResults:   identityTransform(info.decl.Type.Results),
			ResolveEdits: req.resolveEdits(),
		})
		req.addCommandAction(cmd, true)
	}
	return nil
}

func refactorRewriteMoveParamLeft(ctx context.Context, req *codeActionsRequest) error {
	if info := findParam(req.pgf, req.loc.Range); info != nil &&
		info.paramIndex > 0 &&
		!is[*ast.Ellipsis](info.field.Type) {

		// ^^ we can't currently handle moving a variadic param.
		// TODO(rfindley): implement.

		transform := identityTransform(info.decl.Type.Params)
		transform[info.paramIndex] = command.ChangeSignatureParam{OldIndex: info.paramIndex - 1}
		transform[info.paramIndex-1] = command.ChangeSignatureParam{OldIndex: info.paramIndex}
		cmd := command.NewChangeSignatureCommand("Move parameter left", command.ChangeSignatureArgs{
			Location:     req.loc,
			NewParams:    transform,
			NewResults:   identityTransform(info.decl.Type.Results),
			ResolveEdits: req.resolveEdits(),
		})

		req.addCommandAction(cmd, true)
	}
	return nil
}

func refactorRewriteMoveParamRight(ctx context.Context, req *codeActionsRequest) error {
	if info := findParam(req.pgf, req.loc.Range); info != nil && info.paramIndex >= 0 {
		params := info.decl.Type.Params
		nparams := params.NumFields()
		if info.paramIndex < nparams-1 { // not the last param
			if info.paramIndex == nparams-2 && is[*ast.Ellipsis](params.List[len(params.List)-1].Type) {
				// We can't currently handle moving a variadic param.
				// TODO(rfindley): implement.
				return nil
			}

			transform := identityTransform(info.decl.Type.Params)
			transform[info.paramIndex] = command.ChangeSignatureParam{OldIndex: info.paramIndex + 1}
			transform[info.paramIndex+1] = command.ChangeSignatureParam{OldIndex: info.paramIndex}
			cmd := command.NewChangeSignatureCommand("Move parameter right", command.ChangeSignatureArgs{
				Location:     req.loc,
				NewParams:    transform,
				NewResults:   identityTransform(info.decl.Type.Results),
				ResolveEdits: req.resolveEdits(),
			})
			req.addCommandAction(cmd, true)
		}
	}
	return nil
}

// refactorRewriteChangeQuote produces "Convert to {raw,interpreted} string literal" code actions.
func refactorRewriteChangeQuote(ctx context.Context, req *codeActionsRequest) error {
	convertStringLiteral(req)
	return nil
}

// refactorRewriteInvertIf produces "Invert 'if' condition" code actions.
// See [invertIfCondition] for command implementation.
func refactorRewriteInvertIf(ctx context.Context, req *codeActionsRequest) error {
	if _, ok, _ := canInvertIfCondition(req.pgf.Cursor, req.start, req.end); ok {
		req.addApplyFixAction("Invert 'if' condition", fixInvertIfCondition, req.loc)
	}
	return nil
}

// refactorRewriteSplitLines produces "Split ITEMS into separate lines" code actions.
// See [splitLines] for command implementation.
func refactorRewriteSplitLines(ctx context.Context, req *codeActionsRequest) error {
	// TODO(adonovan): opt: don't set needPkg just for FileSet.
	if msg, ok, _ := canSplitLines(req.pgf.Cursor, req.pkg.FileSet(), req.start, req.end); ok {
		req.addApplyFixAction(msg, fixSplitLines, req.loc)
	}
	return nil
}

func refactorRewriteEliminateDotImport(ctx context.Context, req *codeActionsRequest) error {
	// Figure out if the request is placed over a dot import.
	var importSpec *ast.ImportSpec
	for _, imp := range req.pgf.File.Imports {
		if posRangeContains(imp.Pos(), imp.End(), req.start, req.end) {
			importSpec = imp
			break
		}
	}
	if importSpec == nil {
		return nil
	}
	if importSpec.Name == nil || importSpec.Name.Name != "." {
		return nil
	}

	// dotImported package path and its imported name after removing the dot.
	imported := req.pkg.TypesInfo().PkgNameOf(importSpec).Imported()
	newName := imported.Name()

	rng, err := req.pgf.PosRange(importSpec.Name.Pos(), importSpec.Path.Pos())
	if err != nil {
		return err
	}
	// Delete the '.' part of the import.
	edits := []protocol.TextEdit{{
		Range: rng,
	}}

	fileScope, ok := req.pkg.TypesInfo().Scopes[req.pgf.File]
	if !ok {
		return nil
	}

	// Go through each use of the dot imported package, checking its scope for
	// shadowing and calculating an edit to qualify the identifier.
	for curId := range req.pgf.Cursor.Preorder((*ast.Ident)(nil)) {
		ident := curId.Node().(*ast.Ident)

		// Only keep identifiers that use a symbol from the
		// dot imported package.
		use := req.pkg.TypesInfo().Uses[ident]
		if use == nil || use.Pkg() == nil {
			continue
		}
		if use.Pkg() != imported {
			continue
		}

		// Only qualify unqualified identifiers (due to dot imports)
		// that reference package-level symbols.
		// All other references to a symbol imported from another package
		// are nested within a select expression (pkg.Foo, v.Method, v.Field).
		if ek, _ := curId.ParentEdge(); ek == edge.SelectorExpr_Sel {
			continue // qualified identifier (pkg.X) or selector (T.X or e.X)
		}
		if !typesinternal.IsPackageLevel(use) {
			continue // unqualified field reference T{X: ...}
		}

		// Make sure that the package name will not be shadowed by something else in scope.
		// If it is then we cannot offer this particular code action.
		//
		// TODO: If the object found in scope is the package imported without a
		// dot, or some builtin not used in the file, the code action could be
		// allowed to go through.
		sc := fileScope.Innermost(ident.Pos())
		if sc == nil {
			continue
		}
		_, obj := sc.LookupParent(newName, ident.Pos())
		if obj != nil {
			continue
		}

		rng, err := req.pgf.PosRange(ident.Pos(), ident.Pos()) // sic, zero-width range before ident
		if err != nil {
			continue
		}
		edits = append(edits, protocol.TextEdit{
			Range:   rng,
			NewText: newName + ".",
		})
	}

	req.addEditAction("Eliminate dot import", nil, protocol.DocumentChangeEdit(
		req.fh,
		edits,
	))
	return nil
}

// refactorRewriteJoinLines produces "Join ITEMS into one line" code actions.
// See [joinLines] for command implementation.
func refactorRewriteJoinLines(ctx context.Context, req *codeActionsRequest) error {
	// TODO(adonovan): opt: don't set needPkg just for FileSet.
	if msg, ok, _ := canJoinLines(req.pgf.Cursor, req.pkg.FileSet(), req.start, req.end); ok {
		req.addApplyFixAction(msg, fixJoinLines, req.loc)
	}
	return nil
}

// refactorRewriteFillStruct produces "Fill STRUCT" code actions.
// See [fillstruct.SuggestedFix] for command implementation.
func refactorRewriteFillStruct(ctx context.Context, req *codeActionsRequest) error {
	// fillstruct.Diagnose is a lazy analyzer: all it gives us is
	// the (start, end, message) of each SuggestedFix; the actual
	// edit is computed only later by ApplyFix, which calls fillstruct.SuggestedFix.
	for _, diag := range fillstruct.Diagnose(req.pgf.File, req.start, req.end, req.pkg.Types(), req.pkg.TypesInfo()) {
		loc, err := req.pgf.Mapper.PosLocation(req.pgf.Tok, diag.Pos, diag.End)
		if err != nil {
			return err
		}
		for _, fix := range diag.SuggestedFixes {
			req.addApplyFixAction(fix.Message, diag.Category, loc)
		}
	}
	return nil
}

// refactorRewriteFillSwitch produces "Add cases for TYPE/ENUM" code actions.
func refactorRewriteFillSwitch(ctx context.Context, req *codeActionsRequest) error {
	for _, diag := range fillswitch.Diagnose(req.pgf.File, req.start, req.end, req.pkg.Types(), req.pkg.TypesInfo()) {
		changes, err := suggestedFixToDocumentChange(ctx, req.snapshot, req.pkg.FileSet(), &diag.SuggestedFixes[0])
		if err != nil {
			return err
		}
		req.addEditAction(diag.Message, nil, changes...)
	}

	return nil
}

// selectionContainsStructField returns true if the given struct contains a
// field between start and end pos. If needsTag is true, it only returns true if
// the struct field found contains a struct tag.
func selectionContainsStructField(node *ast.StructType, start, end token.Pos, needsTag bool) bool {
	for _, field := range node.Fields.List {
		if start <= field.End() && end >= field.Pos() {
			if !needsTag || field.Tag != nil {
				return true
			}
		}
	}
	return false
}

// selectionContainsStruct returns true if there exists a struct containing
// fields within start and end positions. If removeTags is true, it means the
// current command is for remove tags rather than add tags, so we only return
// true if the struct field found contains a struct tag to remove.
func selectionContainsStruct(cursor inspector.Cursor, start, end token.Pos, removeTags bool) bool {
	cur, ok := cursor.FindByPos(start, end)
	if !ok {
		return false
	}
	if _, ok := cur.Node().(*ast.StructType); ok {
		return true
	}

	// Handles case where selection is within struct.
	for c := range cur.Enclosing((*ast.StructType)(nil)) {
		if selectionContainsStructField(c.Node().(*ast.StructType), start, end, removeTags) {
			return true
		}
	}

	// Handles case where selection contains struct but may contain other nodes, including other structs.
	for c := range cur.Preorder((*ast.StructType)(nil)) {
		node := c.Node().(*ast.StructType)
		// Check that at least one field is located within the selection. If we are removing tags, that field
		// must also have a struct tag, otherwise we do not provide the code action.
		if selectionContainsStructField(node, start, end, removeTags) {
			return true
		}
	}
	return false
}

// refactorRewriteAddStructTags produces "Add struct tags" code actions.
// See [server.commandHandler.ModifyTags] for command implementation.
func refactorRewriteAddStructTags(ctx context.Context, req *codeActionsRequest) error {
	if selectionContainsStruct(req.pgf.Cursor, req.start, req.end, false) {
		// TODO(mkalil): Prompt user for modification args once we have dialogue capabilities.
		cmdAdd := command.NewModifyTagsCommand("Add struct tags", command.ModifyTagsArgs{
			URI:   req.loc.URI,
			Range: req.loc.Range,
			Add:   "json",
		})
		req.addCommandAction(cmdAdd, false)
	}
	return nil
}

// refactorRewriteRemoveStructTags produces "Remove struct tags" code actions.
// See [server.commandHandler.ModifyTags] for command implementation.
func refactorRewriteRemoveStructTags(ctx context.Context, req *codeActionsRequest) error {
	// TODO(mkalil): Prompt user for modification args once we have dialogue capabilities.
	if selectionContainsStruct(req.pgf.Cursor, req.start, req.end, true) {
		cmdRemove := command.NewModifyTagsCommand("Remove struct tags", command.ModifyTagsArgs{
			URI:   req.loc.URI,
			Range: req.loc.Range,
			Clear: true,
		})
		req.addCommandAction(cmdRemove, false)
	}
	return nil
}

// removableParameter returns paramInfo about a removable parameter indicated
// by the given [start, end) range, or nil if no such removal is available.
//
// Removing a parameter is possible if
//   - there are no parse or type errors, and
//   - [start, end) is contained within an unused field or parameter name
//   - ... of a non-method function declaration.
//
// (Note that the unusedparam analyzer also computes this property, but
// much more precisely, allowing it to report its findings as diagnostics.)
//
// TODO(adonovan): inline into refactorRewriteRemoveUnusedParam.
func removableParameter(pkg *cache.Package, pgf *parsego.File, rng protocol.Range) *paramInfo {
	if perrors, terrors := pkg.ParseErrors(), pkg.TypeErrors(); len(perrors) > 0 || len(terrors) > 0 {
		return nil // can't remove parameters from packages with errors
	}
	info := findParam(pgf, rng)
	if info == nil || info.field == nil {
		return nil // range does not span a parameter
	}
	if info.decl.Body == nil {
		return nil // external function
	}
	if len(info.field.Names) == 0 {
		return info // no names => field is unused
	}
	if info.name == nil {
		return nil // no name is indicated
	}
	if info.name.Name == "_" {
		return info // trivially unused
	}

	obj := pkg.TypesInfo().Defs[info.name]
	if obj == nil {
		return nil // something went wrong
	}

	used := false
	ast.Inspect(info.decl.Body, func(node ast.Node) bool {
		if n, ok := node.(*ast.Ident); ok && pkg.TypesInfo().Uses[n] == obj {
			used = true
		}
		return !used // keep going until we find a use
	})
	if used {
		return nil
	}
	return info
}

// refactorInlineCall produces "Inline call to FUNC" code actions.
// See [inlineCall] for command implementation.
func refactorInlineCall(ctx context.Context, req *codeActionsRequest) error {
	// To avoid distraction (e.g. VS Code lightbulb), offer "inline"
	// only after a selection or explicit menu operation.
	// TODO(adonovan): remove this (and req.trigger); see comment at TestVSCodeIssue65167.
	if req.trigger == protocol.CodeActionAutomatic && req.loc.Empty() {
		return nil
	}

	// If range is within call expression, offer to inline the call.
	if _, fn, err := enclosingStaticCall(req.pkg, req.pgf, req.start, req.end); err == nil {
		req.addApplyFixAction("Inline call to "+fn.Name(), fixInlineCall, req.loc)
	}
	return nil
}

// refactorInlineVariable produces the "Inline variable 'v'" code action.
// See [inlineVariableOne] for command implementation.
func refactorInlineVariable(ctx context.Context, req *codeActionsRequest) error {
	// TODO(adonovan): offer "inline all" variant that eliminates the var (see #70085).
	if curUse, _, ok := canInlineVariable(req.pkg.TypesInfo(), req.pgf.Cursor, req.start, req.end); ok {
		title := fmt.Sprintf("Inline variable %q", curUse.Node().(*ast.Ident).Name)
		req.addApplyFixAction(title, fixInlineVariable, req.loc)
	}
	return nil
}

// goTest produces "Run tests and benchmarks" code actions.
// See [server.commandHandler.runTests] for command implementation.
func goTest(ctx context.Context, req *codeActionsRequest) error {
	testFuncs, benchFuncs, err := testsAndBenchmarks(req.pkg.TypesInfo(), req.pgf)
	if err != nil {
		return err
	}

	var tests, benchmarks []string
	for _, fn := range testFuncs {
		if protocol.Intersect(fn.rng, req.loc.Range) {
			tests = append(tests, fn.name)
		}
	}
	for _, fn := range benchFuncs {
		if protocol.Intersect(fn.rng, req.loc.Range) {
			benchmarks = append(benchmarks, fn.name)
		}
	}

	if len(tests) == 0 && len(benchmarks) == 0 {
		return nil
	}

	cmd := command.NewRunTestsCommand("Run tests and benchmarks", command.RunTestsArgs{
		URI:        req.loc.URI,
		Tests:      tests,
		Benchmarks: benchmarks,
	})
	req.addCommandAction(cmd, false)
	return nil
}

// goAssembly produces "Browse ARCH assembly for FUNC" code actions.
// See [server.commandHandler.Assembly] for command implementation.
func goAssembly(ctx context.Context, req *codeActionsRequest) error {
	view := req.snapshot.View()

	// Find the enclosing toplevel function or method,
	// and compute its symbol name (e.g. "pkgpath.(T).method").
	// The report will show this method and all its nested
	// functions (FuncLit, defers, etc).
	//
	// TODO(adonovan): this is no good for generics, since they
	// will always be uninstantiated when they enclose the cursor.
	// Instead, we need to query the func symbol under the cursor,
	// rather than the enclosing function. It may be an explicitly
	// or implicitly instantiated generic, and it may be defined
	// in another package, though we would still need to compile
	// the current package to see its assembly. The challenge,
	// however, is that computing the linker name for a generic
	// symbol is quite tricky. Talk with the compiler team for
	// ideas.
	//
	// TODO(adonovan): think about a smoother UX for jumping
	// directly to (say) a lambda of interest.
	// Perhaps we could scroll to STEXT for the innermost
	// enclosing nested function?

	// Compute the linker symbol of the enclosing function or var initializer.
	var sym strings.Builder
	if pkg := req.pkg.Types(); pkg.Name() == "main" {
		sym.WriteString("main")
	} else {
		sym.WriteString(pkg.Path())
	}
	sym.WriteString(".")

	curSel, _ := req.pgf.Cursor.FindByPos(req.start, req.end)
	for cur := range curSel.Enclosing((*ast.FuncDecl)(nil), (*ast.ValueSpec)(nil)) {
		var name string // in command title
		switch node := cur.Node().(type) {
		case *ast.FuncDecl:
			// package-level func or method
			if fn, ok := req.pkg.TypesInfo().Defs[node.Name].(*types.Func); ok &&
				fn.Name() != "_" { // blank functions are not compiled

				// Source-level init functions are compiled (along with
				// package-level var initializers) in into a single pkg.init
				// function, so this falls out of the logic below.

				if sig := fn.Signature(); sig.TypeParams() == nil && sig.RecvTypeParams() == nil { // generic => no assembly
					if sig.Recv() != nil {
						if isPtr, named := typesinternal.ReceiverNamed(sig.Recv()); named != nil {
							if isPtr {
								fmt.Fprintf(&sym, "(*%s)", named.Obj().Name())
							} else {
								sym.WriteString(named.Obj().Name())
							}
							sym.WriteByte('.')
						}
					}
					sym.WriteString(fn.Name())

					name = node.Name.Name // success
				}
			}

		case *ast.ValueSpec:
			// package-level var initializer?
			if len(node.Names) > 0 && len(node.Values) > 0 {
				v := req.pkg.TypesInfo().Defs[node.Names[0]]
				if v != nil && typesinternal.IsPackageLevel(v) {
					sym.WriteString("init")
					name = "package initializer" // success
				}
			}
		}

		if name != "" {
			cmd := command.NewAssemblyCommand(
				fmt.Sprintf("Browse %s assembly for %s", view.GOARCH(), name),
				view.ID(),
				string(req.pkg.Metadata().ID),
				sym.String())
			req.addCommandAction(cmd, false)
			break
		}
	}
	return nil
}

// toggleCompilerOptDetails produces "{Show,Hide} compiler optimization details" code action.
// See [server.commandHandler.GCDetails] for command implementation.
func toggleCompilerOptDetails(ctx context.Context, req *codeActionsRequest) error {
	// TODO(adonovan): errors from code action providers should probably be
	// logged, even if they aren't visible to the client; see https://go.dev/issue/71275.
	if meta, err := req.snapshot.NarrowestMetadataForFile(ctx, req.fh.URI()); err == nil {
		if len(meta.CompiledGoFiles) == 0 {
			return fmt.Errorf("package %q does not compile file %q", meta.ID, req.fh.URI())
		}
		dir := meta.CompiledGoFiles[0].Dir()

		title := fmt.Sprintf("%s compiler optimization details for %q",
			cond(req.snapshot.WantCompilerOptDetails(dir), "Hide", "Show"),
			dir.Base())
		cmd := command.NewGCDetailsCommand(title, req.fh.URI())
		req.addCommandAction(cmd, false)
	}
	return nil
}
