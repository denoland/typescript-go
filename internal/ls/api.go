package ls

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/microsoft/typescript-go/internal/ast"
	"github.com/microsoft/typescript-go/internal/astnav"
	"github.com/microsoft/typescript-go/internal/checker"
)

var (
	ErrNoSourceFile      = errors.New("source file not found")
	ErrNoTokenAtPosition = errors.New("no token found at position")
)

func (l *LanguageService) GetSymbolAtPosition(ctx context.Context, fileName string, position int) (*ast.Symbol, error) {
	program, file := l.tryGetProgramAndFile(fileName)
	if file == nil {
		return nil, fmt.Errorf("%w: %s", ErrNoSourceFile, fileName)
	}
	node := astnav.GetTokenAtPosition(file, position)
	if node == nil {
		return nil, fmt.Errorf("%w: %s:%d", ErrNoTokenAtPosition, fileName, position)
	}
	checker, done := program.GetTypeCheckerForFile(ctx, file)
	defer done()
	return checker.GetSymbolAtLocation(node), nil
}

func (l *LanguageService) GetSymbolAtLocation(ctx context.Context, node *ast.Node) *ast.Symbol {
	program := l.GetProgram()
	checker, done := program.GetTypeCheckerForFile(ctx, ast.GetSourceFileOfNode(node))
	defer done()
	return checker.GetSymbolAtLocation(node)
}

func (l *LanguageService) GetTypeOfSymbol(ctx context.Context, symbol *ast.Symbol) *checker.Type {
	program := l.GetProgram()
	checker, done := program.GetTypeChecker(ctx)
	defer done()
	return checker.GetTypeOfSymbolAtLocation(symbol, nil)
}

type DiagnosticId uint32

type Diagnostic struct {
	Id DiagnosticId
	FileName string
	Pos int32
	End int32
	Code int32
	Category string
	Message string
	MessageChain []DiagnosticId
	RelatedInformation []DiagnosticId
	ReportsUnnecessary bool
	ReportsDeprecated bool
	SkippedOnNoEmit bool
}

type diagnosticMaps struct {
	diagnosticMapById map[DiagnosticId]*Diagnostic
	diagnosticReverseMap map[*ast.Diagnostic]DiagnosticId
}

func (d *diagnosticMaps) addDiagnostic(diagnostic *ast.Diagnostic) DiagnosticId {
	if i, ok := d.diagnosticReverseMap[diagnostic]; ok {
		return i
	}
	id := DiagnosticId(len(d.diagnosticMapById) + 1)

	diag := &Diagnostic{
		Id: id,
		FileName: diagnostic.File().FileName(),
		Pos: int32(diagnostic.Loc().Pos()),
		End: int32(diagnostic.Loc().End()),
		Code: diagnostic.Code(),
		Category: diagnostic.Category().Name(),
		Message: diagnostic.Message(),
		MessageChain: make([]DiagnosticId, 0, len(diagnostic.MessageChain())),
		RelatedInformation: make([]DiagnosticId, 0, len(diagnostic.RelatedInformation())),
	}
	
	d.diagnosticReverseMap[diagnostic] = id
	
	for _, messageChain := range diagnostic.MessageChain() {
		diag.MessageChain = append(diag.MessageChain, d.addDiagnostic(messageChain))
	}
	
	for _, relatedInformation := range diagnostic.RelatedInformation() {
		diag.RelatedInformation = append(diag.RelatedInformation, d.addDiagnostic(relatedInformation))
	}
	
	d.diagnosticMapById[id] = diag
	return id
}

func (d *diagnosticMaps) GetDiagnostics() []*Diagnostic {
	diagnostics := make([]*Diagnostic, 0, len(d.diagnosticMapById))
	for _, diagnostic := range d.diagnosticMapById {
		diagnostics = append(diagnostics, diagnostic)
	}

	slices.SortFunc(diagnostics, func(a, b *Diagnostic) int {
		return int(int64(a.Id) - int64(b.Id))
	})
	return diagnostics
}

func (l *LanguageService) GetDiagnostics(ctx context.Context) []*Diagnostic {
	program := l.GetProgram()
	sourceFiles := program.GetSourceFiles()
	program.CheckSourceFiles(ctx, sourceFiles)
	diagnosticMaps := &diagnosticMaps{
		diagnosticMapById: make(map[DiagnosticId]*Diagnostic),
		diagnosticReverseMap: make(map[*ast.Diagnostic]DiagnosticId),
	}
	for _, sourceFile := range sourceFiles {
		for _, diagnostic := range program.GetSyntacticDiagnostics(ctx, sourceFile) {
			diagnosticMaps.addDiagnostic(diagnostic)
		}
		for _, diagnostic := range program.GetSemanticDiagnostics(ctx, sourceFile) {
			diagnosticMaps.addDiagnostic(diagnostic)
		}
	}
	return diagnosticMaps.GetDiagnostics()
}
