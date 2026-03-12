package api

import (
	"context"
	"fmt"
	"io"

	"github.com/go-json-experiment/json"
	"github.com/microsoft/typescript-go/internal/ast"
	"github.com/microsoft/typescript-go/internal/bundled"
	"github.com/microsoft/typescript-go/internal/collections"
	"github.com/microsoft/typescript-go/internal/core"
	"github.com/microsoft/typescript-go/internal/diagnostics"
	"github.com/microsoft/typescript-go/internal/lsp/lsproto"
	"github.com/microsoft/typescript-go/internal/module"
	"github.com/microsoft/typescript-go/internal/packagejson"
	"github.com/microsoft/typescript-go/internal/project"
	"github.com/microsoft/typescript-go/internal/project/logging"
	"github.com/microsoft/typescript-go/internal/tsoptions"
	"github.com/microsoft/typescript-go/internal/tspath"
	"github.com/microsoft/typescript-go/internal/vfs"
	"github.com/microsoft/typescript-go/internal/vfs/osvfs"
)

type Callback int

// StdioServerOptions configures the STDIO-based API server.
type StdioServerOptions struct {
	In                 io.ReadCloser
	Out                io.WriteCloser
	Err                io.Writer
	Cwd                string
	DefaultLibraryPath string
	// PipePath, if set, listens on a named pipe (Windows) or Unix domain
	// socket instead of using In/Out for communication.
	PipePath string
	// Callbacks specifies which filesystem operations should be delegated
	// to the client (e.g., "readFile", "fileExists"). Empty means no callbacks.
	Callbacks []string
	// Async enables JSON-RPC protocol with async connection handling.
	// When false (default), uses MessagePack protocol with sync connection.
	Async      bool
	LogEnabled bool
}

// StdioServer runs an API session over STDIO using MessagePack protocol.
// This is the entry point for the synchronous STDIO-based API used by
// native TypeScript tooling integration.
type StdioServer struct {
	options *StdioServerOptions
}

type hostWrapper struct {
	inner              project.ProjectHost
	fs                 *callbackFS
	forkContextInfoPtr **ast.DenoForkContextInfo
}

// CompilerFS implements project.ProjectHost.
func (h *hostWrapper) CompilerFS() *project.CompilerFS {
	return h.inner.CompilerFS()
}

// DefaultLibraryPath implements project.ProjectHost.
func (h *hostWrapper) DefaultLibraryPath() string {
	return h.inner.DefaultLibraryPath()
}

// FS implements project.ProjectHost.
func (h *hostWrapper) FS() vfs.FS {
	return h.inner.FS()
}

// Freeze implements project.ProjectHost.
func (h *hostWrapper) Freeze(snapshotFS *project.SnapshotFS, configFileRegistry *project.ConfigFileRegistry) {
	h.inner.Freeze(snapshotFS, configFileRegistry)
}

// GetCurrentDirectory implements project.ProjectHost.
func (h *hostWrapper) GetCurrentDirectory() string {
	return h.inner.GetCurrentDirectory()
}

// GetResolvedProjectReference implements project.ProjectHost.
func (h *hostWrapper) GetResolvedProjectReference(fileName string, path tspath.Path) *tsoptions.ParsedCommandLine {
	return h.inner.GetResolvedProjectReference(fileName, path)
}

// GetSourceFile implements project.ProjectHost.
func (h *hostWrapper) GetSourceFile(opts ast.SourceFileParseOptions) *ast.SourceFile {
	return h.inner.GetSourceFile(opts)
}

// MakeResolver implements project.ProjectHost.
func (h *hostWrapper) MakeResolver(host module.ResolutionHost, options *core.CompilerOptions, typingsLocation string, projectName string) module.ResolverInterface {
	return newResolverWrapper(h.inner.MakeResolver(host, options, typingsLocation, projectName), h.fs)
}

// SeenFiles implements project.ProjectHost.
func (h *hostWrapper) SeenFiles() *collections.SyncSet[tspath.Path] {
	return h.inner.SeenFiles()
}

// Trace implements project.ProjectHost.
func (h *hostWrapper) Trace(msg *diagnostics.Message, args ...any) {
	h.inner.Trace(msg, args...)
}

// UpdateSeenFiles implements project.ProjectHost.
func (h *hostWrapper) UpdateSeenFiles(seenFiles *collections.SyncSet[tspath.Path]) {
	h.inner.UpdateSeenFiles(seenFiles)
}

var _ project.ProjectHost = (*hostWrapper)(nil)

func (h *hostWrapper) Builder() *project.ProjectCollectionBuilder {
	return h.inner.Builder()
}

func (h *hostWrapper) SessionOptions() *project.SessionOptions {
	return h.inner.SessionOptions()
}

// SourceFS implements project.ProjectHost.
func (h *hostWrapper) SourceFS() *project.SourceFS {
	return h.inner.SourceFS()
}

// TypesNodeIgnorableNames implements project.ProjectHost.
func (h *hostWrapper) GetDenoForkContextInfo() ast.DenoForkContextInfo {
	return **h.forkContextInfoPtr
}

// IsNodeSourceFile implements project.ProjectHost.
func (h *hostWrapper) IsNodeSourceFile(path tspath.Path) bool {
	if h.fs.isEnabled(callbackIsNodeSourceFile) {
		result, err := h.fs.call("isNodeSourceFile", path)
		if err != nil {
			panic(err)
		}
		if len(result) > 0 {
			var res bool
			if err := json.Unmarshal(result, &res); err != nil {
				panic(err)
			}
			return res
		}
	}
	return h.inner.IsNodeSourceFile(path)
}

func newProjectHostWrapper(currentDirectory string, proj *project.Project, builder *project.ProjectCollectionBuilder, logger *logging.LogTree, fs *callbackFS, forkContextInfoPtr **ast.DenoForkContextInfo) *hostWrapper {
	inner := project.NewProjectHost(currentDirectory, proj, builder, logger)
	return &hostWrapper{
		inner:              inner,
		fs:                 fs,
		forkContextInfoPtr: forkContextInfoPtr,
	}
}

type resolverWrapper struct {
	inner module.ResolverInterface
	fs    *callbackFS
}

func newResolverWrapper(inner module.ResolverInterface, fs *callbackFS) *resolverWrapper {
	return &resolverWrapper{
		inner: inner,
		fs:    fs,
	}
}

type PackageJsonIfApplicable struct {
	PackageDirectory string `json:"packageDirectory"`
	DirectoryExists  bool   `json:"directoryExists"`
	Contents         string `json:"contents"`
}

// GetPackageScopeForPath implements module.ResolverInterface.
func (r *resolverWrapper) GetPackageScopeForPath(directory string) *packagejson.InfoCacheEntry {
	if r.fs.isEnabled(callbackGetPackageScopeForPath) {
		result, err := r.fs.call("getPackageScopeForPath", directory)
		if err != nil {
			panic(err)
		}
		if len(result) > 0 {
			var res *PackageJsonIfApplicable
			if err := json.Unmarshal(result, &res); err != nil {
				panic(err)
			}
			if res == nil {
				return nil
			}
			contents, err := packagejson.Parse([]byte(res.Contents))
			if err != nil {
				panic(err)
			}
			return &packagejson.InfoCacheEntry{
				PackageDirectory: res.PackageDirectory,
				DirectoryExists:  res.DirectoryExists,
				Contents: &packagejson.PackageJson{
					Fields: contents,
				},
			}
		}
	}
	return r.inner.GetPackageScopeForPath(directory)
}

// ResolveJsxImportSource implements module.ResolverInterface.
func (r *resolverWrapper) ResolveJsxImportSource(referrerPath string) string {
	if r.fs.isEnabled(callbackResolveJsxImportSource) {
		result, err := r.fs.call("resolveJsxImportSource", referrerPath)
		if err != nil {
			panic(err)
		}
		if len(result) > 0 {
			var res string
			if err := json.Unmarshal(result, &res); err != nil {
				panic(err)
			}
			return res
		}
	}
	return r.inner.ResolveJsxImportSource(referrerPath)
}

// ResolveModuleName implements module.ResolverInterface.
func (r *resolverWrapper) ResolveModuleName(moduleName string, containingFile string, importAttributeType *string, resolutionMode core.ResolutionMode, redirectedReference module.ResolvedProjectReference) (*module.ResolvedModule, []module.DiagAndArgs) {
	if r.fs.isEnabled(callbackResolveModuleName) {
		result, err := r.fs.call("resolveModuleName", map[string]any{
			"moduleName":          moduleName,
			"containingFile":      containingFile,
			"importAttributeType": importAttributeType,
			"resolutionMode":      resolutionMode,
			"redirectedReference": redirectedReference,
		})
		if err != nil {
			panic(err)
		}
		if len(result) > 0 {
			var res module.ResolvedModule
			if err := json.Unmarshal(result, &res); err != nil {
				panic(err)
			}
			return &res, nil
		}
	}
	return r.inner.ResolveModuleName(moduleName, containingFile, importAttributeType, resolutionMode, redirectedReference)
}

// ResolveModuleName implements module.ResolverInterface.
func (r *resolverWrapper) ResolvePackageDirectory(moduleName string, containingFile string, resolutionMode core.ResolutionMode, redirectedReference module.ResolvedProjectReference) *module.ResolvedModule {
	if r.fs.isEnabled(callbackResolveModuleName) {
		result, err := r.fs.call("resolveModuleName", map[string]any{
			"moduleName":          moduleName,
			"containingFile":      containingFile,
			"resolutionMode":      resolutionMode,
			"redirectedReference": redirectedReference,
		})
		if err != nil {
			panic(err)
		}
		if len(result) > 0 {
			var res module.ResolvedModule
			if err := json.Unmarshal(result, &res); err != nil {
				panic(err)
			}
			return &res
		}
	}
	return r.inner.ResolvePackageDirectory(moduleName, containingFile, resolutionMode, redirectedReference)
}

// ResolveTypeReferenceDirective implements module.ResolverInterface.
func (r *resolverWrapper) ResolveTypeReferenceDirective(typeReferenceDirectiveName string, containingFile string, resolutionMode core.ResolutionMode, redirectedReference module.ResolvedProjectReference) (*module.ResolvedTypeReferenceDirective, []module.DiagAndArgs) {
	if r.fs.isEnabled(callbackResolveTypeReferenceDirective) {
		result, err := r.fs.call("resolveTypeReferenceDirective", map[string]any{
			"typeReferenceDirectiveName": typeReferenceDirectiveName,
			"containingFile":             containingFile,
			"resolutionMode":             resolutionMode,
			"redirectedReference":        redirectedReference,
		})
		if err != nil {
			panic(err)
		}
		if len(result) > 0 {
			var res module.ResolvedTypeReferenceDirective
			if err := json.Unmarshal(result, &res); err != nil {
				panic(err)
			}
			return &res, nil
		}
	}
	return r.inner.ResolveTypeReferenceDirective(typeReferenceDirectiveName, containingFile, resolutionMode, redirectedReference)
}

func (r *resolverWrapper) GetImpliedNodeFormatForFile(path string, packageJsonType string) core.ModuleKind {
	if r.fs.isEnabled(callbackGetImpliedNodeFormatForFile) {
		result, err := r.fs.call("getImpliedNodeFormatForFile", map[string]any{
			"fileName":        path,
			"packageJsonType": packageJsonType,
		})
		if err != nil {
			panic(err)
		}
		if len(result) > 0 {
			var res core.ModuleKind
			if err := json.Unmarshal(result, &res); err != nil {
				panic(err)
			}
			return res
		}
	}
	return r.inner.GetImpliedNodeFormatForFile(path, packageJsonType)
}

var _ module.ResolverInterface = (*resolverWrapper)(nil)

// NewStdioServer creates a new STDIO-based API server.
func NewStdioServer(options *StdioServerOptions) *StdioServer {
	if options.Cwd == "" {
		panic("StdioServerOptions.Cwd is required")
	}

	return &StdioServer{
		options: options,
	}
}

// Run starts the server and blocks until the connection closes.
func (s *StdioServer) Run(ctx context.Context) error {
	var transport Transport
	if s.options.PipePath != "" {
		t, err := NewPipeTransport(s.options.PipePath)
		if err != nil {
			return fmt.Errorf("failed to create pipe transport: %w", err)
		}
		defer t.Close()
		transport = t
	} else {
		t := NewStdioTransport(s.options.In, s.options.Out)
		defer t.Close()
		transport = t
	}

	fs := bundled.WrapFS(osvfs.FS())

	// Wrap the base FS with callbackFS if callbacks are requested
	var callbackFS *callbackFS
	if len(s.options.Callbacks) > 0 {
		callbackFS = newCallbackFS(fs, s.options.Callbacks)
		fs = callbackFS
	}

	var forkContextInfo *ast.DenoForkContextInfo = nil
	forkContextInfoPtr := &forkContextInfo

	projectSession := project.NewSession(&project.SessionInit{
		BackgroundCtx: ctx,
		Logger:        nil, // TODO: Add logging support
		FS:            fs,
		Options: &project.SessionOptions{
			CurrentDirectory:   s.options.Cwd,
			DefaultLibraryPath: s.options.DefaultLibraryPath,
			PositionEncoding:   lsproto.PositionEncodingKindUTF8,
			LoggingEnabled:     false,
			MakeHost: func(currentDirectory string, proj *project.Project, builder *project.ProjectCollectionBuilder, logger *logging.LogTree) project.ProjectHost {
				return newProjectHostWrapper(currentDirectory, proj, builder, logger, callbackFS, forkContextInfoPtr)
			},
		},
	})

	session := NewSession(projectSession, &SessionOptions{
		UseBinaryResponses: !s.options.Async, // Only msgpack uses binary responses
		SetForkContextInfo: func(value ast.DenoForkContextInfo) {
			*forkContextInfoPtr = &value
		},
	})
	defer session.Close()

	// Accept connection from transport
	rwc, err := transport.Accept()
	if err != nil {
		return fmt.Errorf("failed to accept connection: %w", err)
	}
	// Create protocol and connection based on async mode
	var conn Conn
	if s.options.Async {
		protocol := NewJSONRPCProtocol(rwc)
		conn = NewAsyncConnWithProtocol(rwc, protocol, session)
	} else {
		protocol := NewMessagePackProtocol(rwc)
		conn = NewSyncConn(rwc, protocol, session)
	}

	// If callbacks are enabled, set the connection on the FS
	if callbackFS != nil {
		callbackFS.SetConnection(ctx, conn)
	}

	return conn.Run(ctx)
}
