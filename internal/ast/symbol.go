package ast

import (
	"iter"
	"maps"
	"sync/atomic"

	"github.com/microsoft/typescript-go/internal/collections"
)

// Symbol

type Symbol struct {
	Flags                        SymbolFlags
	CheckFlags                   CheckFlags // Non-zero only in transient symbols created by Checker
	Name                         string
	Declarations                 []*Node
	ValueDeclaration             *Node
	Members                      SymbolTable
	Exports                      SymbolTable
	id                           atomic.Uint64
	Parent                       *Symbol
	ExportSymbol                 *Symbol
	AssignmentDeclarationMembers collections.Set[*Node] // Set of detected assignment declarations
	GlobalExports                SymbolTable            // Conditional global UMD exports
}

// SymbolTable

// type SymbolTable map[string]*Symbol

type SymbolTable interface {
	Get(name string) *Symbol
	Get2(name string) (*Symbol, bool)
	Set(name string, symbol *Symbol)
	Delete(name string)
	Keys() iter.Seq[string]
	Values() iter.Seq[*Symbol]
	Each(func(name string, symbol *Symbol) )
	Iter() iter.Seq2[string, *Symbol]
	Len() int
	Clone() SymbolTable
	Find(predicate func(*Symbol) bool) *Symbol
	
}

type SymbolMap struct {
	m map[string]*Symbol
}

func (m *SymbolMap) Find(predicate func(*Symbol) bool) *Symbol {
	for _, symbol := range m.m {
		if predicate(symbol) {
			return symbol
		}
	}
	return nil
}

func (m *SymbolMap) Clone() SymbolTable {
	return &SymbolMap{m: maps.Clone(m.m)}
}

func (m *SymbolMap) Len() int {
	return len(m.m)
}

func (m *SymbolMap) Iter() iter.Seq2[string, *Symbol] {
	return func(yield func (string, *Symbol) bool) {
		for name, symbol := range m.m {
			if !yield(name, symbol) {
				return
			}
		}
	}
}

func (m *SymbolMap) Get(name string) *Symbol {
	return m.m[name]
}

func (m *SymbolMap) Get2(name string) (*Symbol, bool) {
	symbol, ok := m.m[name]
	return symbol, ok
}

func (m *SymbolMap) Set(name string, symbol *Symbol) {
	m.m[name] = symbol
}

func (m *SymbolMap) Delete(name string) {
	delete(m.m, name)
}

func (m *SymbolMap) Keys() iter.Seq[string] {
	return func(yield func (string) bool) {
		for name := range m.m {
			if !yield(name) {
				return
			}
		}
	}
}

func (m *SymbolMap) Values() iter.Seq[*Symbol] {
	return func(yield func (*Symbol) bool) {
		for _, symbol := range m.m {
			if !yield(symbol) {
				return
			}
		}
	}
}

func (m *SymbolMap) Each(fn func(name string, symbol *Symbol) ) {
	for name, symbol := range m.m {
		fn(name, symbol)
	}
}

func NewSymbolTable() SymbolTable {
	return &SymbolMap{m: make(map[string]*Symbol)}
}

func NewSymbolTableWithCapacity(capacity int) SymbolTable {
	return &SymbolMap{m: make(map[string]*Symbol, capacity)}
}

func NewSymbolTableFromMap(m map[string]*Symbol) SymbolTable {
	return &SymbolMap{m: m}
}


const InternalSymbolNamePrefix = "\xFE" // Invalid UTF8 sequence, will never occur as IdentifierName

const (
	InternalSymbolNameCall                    = InternalSymbolNamePrefix + "call"                    // Call signatures
	InternalSymbolNameConstructor             = InternalSymbolNamePrefix + "constructor"             // Constructor implementations
	InternalSymbolNameNew                     = InternalSymbolNamePrefix + "new"                     // Constructor signatures
	InternalSymbolNameIndex                   = InternalSymbolNamePrefix + "index"                   // Index signatures
	InternalSymbolNameExportStar              = InternalSymbolNamePrefix + "export"                  // Module export * declarations
	InternalSymbolNameGlobal                  = InternalSymbolNamePrefix + "global"                  // Global self-reference
	InternalSymbolNameMissing                 = InternalSymbolNamePrefix + "missing"                 // Indicates missing symbol
	InternalSymbolNameType                    = InternalSymbolNamePrefix + "type"                    // Anonymous type literal symbol
	InternalSymbolNameObject                  = InternalSymbolNamePrefix + "object"                  // Anonymous object literal declaration
	InternalSymbolNameJSXAttributes           = InternalSymbolNamePrefix + "jsxAttributes"           // Anonymous JSX attributes object literal declaration
	InternalSymbolNameClass                   = InternalSymbolNamePrefix + "class"                   // Unnamed class expression
	InternalSymbolNameFunction                = InternalSymbolNamePrefix + "function"                // Unnamed function expression
	InternalSymbolNameComputed                = InternalSymbolNamePrefix + "computed"                // Computed property name declaration with dynamic name
	InternalSymbolNameResolving               = InternalSymbolNamePrefix + "resolving"               // Indicator symbol used to mark partially resolved type aliases
	InternalSymbolNameInstantiationExpression = InternalSymbolNamePrefix + "instantiationExpression" // Instantiation expressions
	InternalSymbolNameImportAttributes        = InternalSymbolNamePrefix + "importAttributes"
	InternalSymbolNameExportEquals            = "export=" // Export assignment symbol
	InternalSymbolNameDefault                 = "default" // Default export symbol (technically not wholly internal, but included here for usability)
	InternalSymbolNameThis                    = "this"
	InternalSymbolNameModuleExports           = "module.exports"
)

func SymbolName(symbol *Symbol) string {
	if symbol.ValueDeclaration != nil && IsPrivateIdentifierClassElementDeclaration(symbol.ValueDeclaration) {
		return symbol.ValueDeclaration.Name().Text()
	}
	return symbol.Name
}
