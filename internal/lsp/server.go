package lsp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"math/rand/v2"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-json-experiment/json/jsontext"
	"github.com/microsoft/typescript-go/internal/api"
	"github.com/microsoft/typescript-go/internal/ast"
	"github.com/microsoft/typescript-go/internal/bundled"
	"github.com/microsoft/typescript-go/internal/collections"
	"github.com/microsoft/typescript-go/internal/compiler"
	"github.com/microsoft/typescript-go/internal/core"
	"github.com/microsoft/typescript-go/internal/diagnostics"
	"github.com/microsoft/typescript-go/internal/json"
	"github.com/microsoft/typescript-go/internal/jsonrpc"
	"github.com/microsoft/typescript-go/internal/locale"
	"github.com/microsoft/typescript-go/internal/ls"
	"github.com/microsoft/typescript-go/internal/ls/autoimport"
	"github.com/microsoft/typescript-go/internal/ls/lsconv"
	"github.com/microsoft/typescript-go/internal/ls/lsutil"
	"github.com/microsoft/typescript-go/internal/lsp/lsproto"
	"github.com/microsoft/typescript-go/internal/module"
	"github.com/microsoft/typescript-go/internal/packagejson"
	"github.com/microsoft/typescript-go/internal/parser"
	"github.com/microsoft/typescript-go/internal/pprof"
	"github.com/microsoft/typescript-go/internal/project"
	"github.com/microsoft/typescript-go/internal/project/ata"
	"github.com/microsoft/typescript-go/internal/sourcemap"
	"github.com/microsoft/typescript-go/internal/tsoptions"
	"github.com/microsoft/typescript-go/internal/tspath"
	"github.com/microsoft/typescript-go/internal/vfs"
	"golang.org/x/sync/errgroup"
)

type ServerOptions struct {
	In  Reader
	Out Writer
	Err io.Writer

	Cwd                string
	FS                 vfs.FS
	DefaultLibraryPath string
	TypingsLocation    string
	ParseCache         *project.ParseCache
	NpmInstall         func(cwd string, args []string) ([]byte, error)
}

func NewServer(opts *ServerOptions) *Server {
	if opts.Cwd == "" {
		panic("Cwd is required")
	}

	s := &Server{
		r:                     opts.In,
		w:                     opts.Out,
		stderr:                opts.Err,
		requestQueue:          make(chan *lsproto.RequestMessage, 100),
		outgoingQueue:         make(chan *lsproto.Message, 100),
		pendingClientRequests: make(map[jsonrpc.ID]pendingClientRequest),
		pendingServerRequests: make(map[jsonrpc.ID]chan *lsproto.ResponseMessage),
		cwd:                   opts.Cwd,
		fs:                    opts.FS,
		defaultLibraryPath:    opts.DefaultLibraryPath,
		typingsLocation:       opts.TypingsLocation,
		parseCache:            opts.ParseCache,
		npmInstall:            opts.NpmInstall,
		initComplete:          make(chan struct{}),
	}
	s.logger = newLogger(s)

	return s
}

var (
	_ ata.NpmExecutor = (*Server)(nil)
	_ project.Client  = (*Server)(nil)
)

type pendingClientRequest struct {
	req    *lsproto.RequestMessage
	cancel context.CancelFunc
}

type Reader interface {
	Read() (*lsproto.Message, error)
}

type Writer interface {
	Write(msg *lsproto.Message) error
}

type lspReader struct {
	r *lsproto.BaseReader
}

type lspWriter struct {
	w *lsproto.BaseWriter
}

func (r *lspReader) Read() (*lsproto.Message, error) {
	data, err := r.r.Read()
	if err != nil {
		return nil, err
	}

	req := &lsproto.Message{}
	if err := json.Unmarshal(data, req); err != nil {
		if errors.Is(err, lsproto.ErrorCodeInvalidParams) {
			return req, fmt.Errorf("%w: %w", lsproto.ErrorCodeInvalidParams, err)
		}
		return nil, fmt.Errorf("%w: %w", lsproto.ErrorCodeInvalidRequest, err)
	}

	return req, nil
}

func ToReader(r io.Reader) Reader {
	return &lspReader{r: lsproto.NewBaseReader(r)}
}

func (w *lspWriter) Write(msg *lsproto.Message) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}
	return w.w.Write(data)
}

func ToWriter(w io.Writer) Writer {
	return &lspWriter{w: lsproto.NewBaseWriter(w)}
}

var (
	_ Reader = (*lspReader)(nil)
	_ Writer = (*lspWriter)(nil)
)

type DenoProgramEntry struct {
	program            *compiler.Program
	projectPath        tspath.Path
	compilerOptionsKey string
	notebookUri        *string
	compilerOptions    *core.CompilerOptions
	userPreferences    *lsutil.UserPreferences
	vfs                *DenoVFS
	autoImportRegistry *autoimport.Registry
}

type DenoProgramEntries struct {
	byCompilerOptionsKey map[string]*DenoProgramEntry
	byNotebookUri        map[string]*DenoProgramEntry
}

type DenoServerData struct {
	programEntries  DenoProgramEntries
	documentCache   map[lsproto.DocumentUri]*lsproto.DenoDocumentData
	documentCacheMu sync.RWMutex
}

type DenoLanguageServiceHost struct {
	vfs                *DenoVFS
	positionEncoding   lsproto.PositionEncodingKind
	userPreferences    *lsutil.UserPreferences
	autoImportRegistry *autoimport.Registry
}

var _ ls.Host = (*DenoLanguageServiceHost)(nil)

func NewDenoLanguageServiceHost(vfs *DenoVFS, userPreferences *lsutil.UserPreferences, autoImportRegistry *autoimport.Registry) *DenoLanguageServiceHost {
	return &DenoLanguageServiceHost{
		vfs:                vfs,
		positionEncoding:   lsproto.PositionEncodingKindUTF16, // default
		userPreferences:    userPreferences,
		autoImportRegistry: autoImportRegistry,
	}
}

func (h *DenoLanguageServiceHost) UseCaseSensitiveFileNames() bool {
	return true
}

func (h *DenoLanguageServiceHost) ReadFile(path string) (contents string, ok bool) {
	return h.vfs.ReadFile(path)
}

func (h *DenoLanguageServiceHost) Converters() *lsconv.Converters {
	return lsconv.NewConverters(h.positionEncoding, h.getLineMap)
}

func (h *DenoLanguageServiceHost) getLineMap(fileName string) *lsconv.LSPLineMap {
	if data := h.vfs.GetDocument(fileName); data != nil {
		return &lsconv.LSPLineMap{
			LineStarts: data.LineStarts,
			AsciiOnly:  data.AsciiOnly,
		}
	}
	return &lsconv.LSPLineMap{}
}

func (h *DenoLanguageServiceHost) GetPreferences(_path string) *lsutil.UserPreferences {
	return h.userPreferences
}

func (h *DenoLanguageServiceHost) GetECMALineInfo(fileName string) *sourcemap.ECMALineInfo {
	if data := h.vfs.GetDocument(fileName); data != nil {
		return sourcemap.CreateECMALineInfo(data.Text, data.LineStarts)
	}
	return nil
}

func (h *DenoLanguageServiceHost) AutoImportRegistry() *autoimport.Registry {
	return h.autoImportRegistry
}

func (h *DenoLanguageServiceHost) ReadDirectory(currentDir string, path string, extensions []string, excludes []string, includes []string, depth *int) []string {
	return make([]string, 0)
}

func (h *DenoLanguageServiceHost) GetDirectories(path string) []string {
	return make([]string, 0)
}

func (h *DenoLanguageServiceHost) DirectoryExists(path string) bool {
	return false
}

// DenoVFS implements vfs.FS by reading files from the client via LSP requests
type DenoVFS struct {
	server             *Server
	ctx                context.Context
	compilerOptionsKey string
	notebookUri        *string
}

var _ vfs.FS = (*DenoVFS)(nil)

func NewDenoVFS(server *Server, ctx context.Context, compilerOptionsKey string, notebookUri *string) *DenoVFS {
	return &DenoVFS{
		server:             server,
		ctx:                ctx,
		compilerOptionsKey: compilerOptionsKey,
		notebookUri:        notebookUri,
	}
}

func (v *DenoVFS) UseCaseSensitiveFileNames() bool {
	return true
}

func (v *DenoVFS) FileExists(path string) bool {
	_, ok := v.ReadFile(path)
	return ok
}

func (v *DenoVFS) GetDocument(path string) *lsproto.DenoDocumentData {
	uri := lsconv.FileNameToDocumentURI(path)
	v.server.deno.documentCacheMu.Lock()
	defer v.server.deno.documentCacheMu.Unlock()
	if v.server.deno.documentCache != nil {
		if data, ok := v.server.deno.documentCache[uri]; ok {
			return data
		}
	}
	params := lsproto.DenoCallbackParams{
		GetDocument: &lsproto.DenoGetDocumentParams{
			Uri: uri,
		},
	}
	var response lsproto.DenoDocumentData
	err := v.server.sendRequestSync(v.ctx, lsproto.MethodDenoCallback, params, &response)
	if err != nil {
		return nil
	}
	if v.server.deno.documentCache != nil {
		v.server.deno.documentCache[uri] = &response
	}
	return &response
}

func (v *DenoVFS) ReadFile(path string) (contents string, ok bool) {
	if data := v.GetDocument(path); data != nil {
		return data.Text, true
	}
	return "", false
}

func (v *DenoVFS) WriteFile(path string, data string) error {
	return fmt.Errorf("DenoVFS does not support WriteFile")
}

func (v *DenoVFS) Remove(path string) error {
	return fmt.Errorf("DenoVFS does not support Remove")
}

func (v *DenoVFS) Chtimes(path string, aTime time.Time, mTime time.Time) error {
	return fmt.Errorf("DenoVFS does not support Chtimes")
}

func (v *DenoVFS) DirectoryExists(path string) bool {
	// Directories are managed by the client
	return false
}

func (v *DenoVFS) GetAccessibleEntries(path string) vfs.Entries {
	return vfs.Entries{}
}

func (v *DenoVFS) Stat(path string) vfs.FileInfo {
	return nil
}

func (v *DenoVFS) WalkDir(root string, walkFn vfs.WalkDirFunc) error {
	return fmt.Errorf("DenoVFS does not support WalkDir")
}

func (v *DenoVFS) Realpath(path string) string {
	return path
}

// denoAutoImportHost is a minimal implementation of autoimport.RegistryCloneHost
// used to initialize the auto-import registry for Deno projects.
type denoAutoImportHost struct {
	vfs         vfs.FS
	cwd         string
	projectPath tspath.Path
	program     *compiler.Program
}

var _ autoimport.RegistryCloneHost = (*denoAutoImportHost)(nil)

func (h *denoAutoImportHost) FS() vfs.FS {
	return h.vfs
}

func (h *denoAutoImportHost) GetCurrentDirectory() string {
	return h.cwd
}

func (h *denoAutoImportHost) GetDefaultProject(path tspath.Path) (tspath.Path, *compiler.Program) {
	return h.projectPath, h.program
}

func (h *denoAutoImportHost) GetProgramForProject(projectPath tspath.Path) *compiler.Program {
	if projectPath == h.projectPath {
		return h.program
	}
	return nil
}

func (h *denoAutoImportHost) GetPackageJson(fileName string) *packagejson.InfoCacheEntry {
	return nil
}

func (h *denoAutoImportHost) GetSourceFile(fileName string, path tspath.Path) *ast.SourceFile {
	return h.program.GetSourceFile(fileName)
}

func (h *denoAutoImportHost) Dispose() {
}

// denoCompilerHost wraps a CompilerHost to intercept resolver creation for Deno LSP
type denoCompilerHost struct {
	inner              compiler.CompilerHost
	server             *Server
	vfs                *DenoVFS
	compilerOptionsKey string
	notebookUri        *string
}

var _ compiler.CompilerHost = (*denoCompilerHost)(nil)

func (h *denoCompilerHost) FS() vfs.FS {
	return h.inner.FS()
}

func (h *denoCompilerHost) DefaultLibraryPath() string {
	return h.inner.DefaultLibraryPath()
}

func (h *denoCompilerHost) GetCurrentDirectory() string {
	return h.inner.GetCurrentDirectory()
}

func (h *denoCompilerHost) Trace(msg *diagnostics.Message, args ...any) {
	h.inner.Trace(msg, args...)
}

func (h *denoCompilerHost) GetSourceFile(opts ast.SourceFileParseOptions) *ast.SourceFile {
	data := h.vfs.GetDocument(opts.FileName)
	if data == nil {
		return nil
	}
	scriptKind := data.ScriptKind
	if scriptKind == core.ScriptKindUnknown {
		scriptKind = core.GetScriptKindFromFileName(opts.FileName)
	}
	sourceFile := parser.ParseSourceFile(opts, data.Text, scriptKind)
	if data.IsNotebookCell {
		sourceFile.ExternalModuleIndicator = nil
	}
	return sourceFile
}

func (h *denoCompilerHost) GetResolvedProjectReference(fileName string, path tspath.Path) *tsoptions.ParsedCommandLine {
	return h.inner.GetResolvedProjectReference(fileName, path)
}

func (h *denoCompilerHost) IsNodeSourceFile(path tspath.Path) bool {
	return h.inner.IsNodeSourceFile(path)
}

func (h *denoCompilerHost) GetDenoForkContextInfo() ast.DenoForkContextInfo {
	return h.inner.GetDenoForkContextInfo()
}

// MakeResolver overrides the inner host's MakeResolver to return a wrapped resolver
func (h *denoCompilerHost) MakeResolver(host module.ResolutionHost, options *core.CompilerOptions,
	typingsLocation string, projectName string,
) module.ResolverInterface {
	baseResolver := h.inner.MakeResolver(host, options, typingsLocation, projectName)
	return &denoResolver{
		inner:              baseResolver,
		server:             h.server,
		ctx:                h.vfs.ctx,
		compilerOptionsKey: h.compilerOptionsKey,
		notebookUri:        h.notebookUri,
	}
}

// denoResolver wraps a ResolverInterface to intercept module resolution for Deno LSP
type denoResolver struct {
	inner              module.ResolverInterface
	server             *Server
	ctx                context.Context
	compilerOptionsKey string
	notebookUri        *string
}

var _ module.ResolverInterface = (*denoResolver)(nil)

func (r *denoResolver) ResolveModuleName(moduleName string, containingFile string,
	importAttributeType *string, resolutionMode core.ResolutionMode,
	redirectedReference module.ResolvedProjectReference,
) (*module.ResolvedModule, []module.DiagAndArgs) {
	params := lsproto.DenoCallbackParams{
		ResolveModuleName: &lsproto.DenoResolveModuleNameParams{
			ModuleName:          moduleName,
			ReferrerUri:         lsconv.FileNameToDocumentURI(containingFile),
			ImportAttributeType: importAttributeType,
			ResolutionMode:      int32(resolutionMode),
			CompilerOptionsKey:  r.compilerOptionsKey,
		},
	}
	var response *lsproto.DenoResolution
	err := r.server.sendRequestSync(r.ctx, lsproto.MethodDenoCallback, params, &response)
	if err != nil || response == nil {
		return &module.ResolvedModule{
			ResolvedFileName: "",
			Extension:        "",
		}, nil
	}
	return &module.ResolvedModule{
		ResolvedFileName: response.Uri.FileName(),
		Extension:        response.Extension,
	}, nil
}

func (r *denoResolver) ResolveTypeReferenceDirective(name string, containingFile string,
	resolutionMode core.ResolutionMode,
	redirectedReference module.ResolvedProjectReference,
) (*module.ResolvedTypeReferenceDirective, []module.DiagAndArgs) {
	params := lsproto.DenoCallbackParams{
		ResolveModuleName: &lsproto.DenoResolveModuleNameParams{
			ModuleName:          name,
			ReferrerUri:         lsconv.FileNameToDocumentURI(containingFile),
			ImportAttributeType: nil,
			ResolutionMode:      int32(resolutionMode),
			CompilerOptionsKey:  r.compilerOptionsKey,
		},
	}
	var response *lsproto.DenoResolution
	err := r.server.sendRequestSync(r.ctx, lsproto.MethodDenoCallback, params, &response)
	if err != nil || response == nil {
		return &module.ResolvedTypeReferenceDirective{
			ResolvedFileName: "",
			Primary:          true,
		}, nil
	}
	return &module.ResolvedTypeReferenceDirective{
		ResolvedFileName: response.Uri.FileName(),
		Primary:          true,
	}, nil
}

func (r *denoResolver) ResolvePackageDirectory(moduleName string, containingFile string,
	resolutionMode core.ResolutionMode,
	redirectedReference module.ResolvedProjectReference,
) *module.ResolvedModule {
	params := lsproto.DenoCallbackParams{
		ResolveModuleName: &lsproto.DenoResolveModuleNameParams{
			ModuleName:          moduleName,
			ReferrerUri:         lsconv.FileNameToDocumentURI(containingFile),
			ImportAttributeType: nil,
			ResolutionMode:      int32(resolutionMode),
			CompilerOptionsKey:  r.compilerOptionsKey,
		},
	}
	var response *lsproto.DenoResolution
	err := r.server.sendRequestSync(r.ctx, lsproto.MethodDenoCallback, params, &response)
	if err != nil || response == nil {
		return &module.ResolvedModule{
			ResolvedFileName: "",
			Extension:        "",
		}
	}
	return &module.ResolvedModule{
		ResolvedFileName: response.Uri.FileName(),
		Extension:        response.Extension,
	}
}

func (r *denoResolver) ResolveJsxImportSource(_referrer string) string {
	params := lsproto.DenoCallbackParams{
		ResolveJsxImportSource: &lsproto.DenoResolveJsxImportSourceParams{
			CompilerOptionsKey: r.compilerOptionsKey,
		},
	}
	var response *string
	err := r.server.sendRequestSync(r.ctx, lsproto.MethodDenoCallback, params, &response)
	if err != nil || response == nil {
		return ""
	}
	return *response
}

func (r *denoResolver) GetPackageScopeForPath(directory string) *packagejson.InfoCacheEntry {
	if tspath.IsDynamicFileName(directory) {
		return nil
	}
	params := lsproto.DenoCallbackParams{
		GetPackageScopeForPath: &lsproto.DenoGetPackageScopeForPathParams{
			DirectoryPath: directory,
		},
	}
	var response *lsproto.DenoPackageJsonScope
	err := r.server.sendRequestSync(r.ctx, lsproto.MethodDenoCallback, params, &response)
	if err != nil || response == nil {
		return nil
	}
	fields, err := packagejson.Parse([]byte(response.PackageJsonText))
	if err != nil {
		return &packagejson.InfoCacheEntry{
			PackageDirectory: response.PackageDirectoryPath,
			DirectoryExists:  true,
			Contents: &packagejson.PackageJson{
				Parseable: false,
			},
		}
	}
	return &packagejson.InfoCacheEntry{
		PackageDirectory: response.PackageDirectoryPath,
		DirectoryExists:  true,
		Contents: &packagejson.PackageJson{
			Fields:    fields,
			Parseable: true,
		},
	}
}

func (r *denoResolver) GetImpliedNodeFormatForFile(path string, packageJsonType string) core.ModuleKind {
	params := lsproto.DenoCallbackParams{
		GetImpliedNodeFormatForFile: &lsproto.DenoGetImpliedNodeFormatForFileParams{
			Uri:                lsconv.FileNameToDocumentURI(path),
			CompilerOptionsKey: r.compilerOptionsKey,
		},
	}
	var response core.ModuleKind
	err := r.server.sendRequestSync(r.ctx, lsproto.MethodDenoCallback, params, &response)
	if err != nil {
		return core.ModuleKindESNext
	}
	return response
}

type Server struct {
	r             Reader
	w             Writer
	backgroundCtx context.Context

	stderr io.Writer

	logger                  *logger
	initStarted             atomic.Bool
	clientSeq               atomic.Int32
	requestQueue            chan *lsproto.RequestMessage
	outgoingQueue           chan *lsproto.Message
	pendingClientRequests   map[jsonrpc.ID]pendingClientRequest
	pendingClientRequestsMu sync.Mutex
	pendingServerRequests   map[jsonrpc.ID]chan *lsproto.ResponseMessage
	pendingServerRequestsMu sync.Mutex

	cwd                string
	fs                 vfs.FS
	defaultLibraryPath string
	typingsLocation    string

	initializeParams   *lsproto.InitializeParams
	clientCapabilities lsproto.ResolvedClientCapabilities
	positionEncoding   lsproto.PositionEncodingKind
	locale             locale.Locale

	watchEnabled bool
	watcherID    atomic.Uint32
	watchers     collections.SyncSet[project.WatcherID]

	session *project.Session

	// apiSessions holds active API sessions keyed by their ID
	apiSessions   map[string]*api.Session
	apiSessionsMu sync.Mutex

	// Test options for initializing session
	client project.Client

	// initComplete is closed when handleInitialized completes.
	// Used by tests to wait for full initialization.
	initComplete chan struct{}

	// !!! temporary; remove when we have `handleDidChangeConfiguration`/implicit project config support
	compilerOptionsForInferredProjects *core.CompilerOptions
	// parseCache can be passed in so separate tests can share ASTs
	parseCache *project.ParseCache

	npmInstall func(cwd string, args []string) ([]byte, error)

	deno DenoServerData

	cpuProfiler pprof.CPUProfiler
}

func (s *Server) Session() *project.Session { return s.session }

// InitComplete returns a channel that is closed when the server has finished
// processing the initialized notification, including the initial configuration
// exchange with the client.
func (s *Server) InitComplete() <-chan struct{} { return s.initComplete }

// WatchFiles implements project.Client.
func (s *Server) WatchFiles(ctx context.Context, id project.WatcherID, watchers []*lsproto.FileSystemWatcher) error {
	_, err := sendClientRequest(ctx, s, lsproto.ClientRegisterCapabilityInfo, &lsproto.RegistrationParams{
		Registrations: []*lsproto.Registration{
			{
				Id:     string(id),
				Method: string(lsproto.MethodWorkspaceDidChangeWatchedFiles),
				RegisterOptions: &lsproto.RegisterOptions{
					DidChangeWatchedFiles: &lsproto.DidChangeWatchedFilesRegistrationOptions{
						Watchers: watchers,
					},
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to register file watcher: %w", err)
	}

	s.watchers.Add(id)
	return nil
}

// UnwatchFiles implements project.Client.
func (s *Server) UnwatchFiles(ctx context.Context, id project.WatcherID) error {
	if s.watchers.Has(id) {
		_, err := sendClientRequest(ctx, s, lsproto.ClientUnregisterCapabilityInfo, &lsproto.UnregistrationParams{
			Unregisterations: []*lsproto.Unregistration{
				{
					Id:     string(id),
					Method: string(lsproto.MethodWorkspaceDidChangeWatchedFiles),
				},
			},
		})
		if err != nil {
			return fmt.Errorf("failed to unregister file watcher: %w", err)
		}

		s.watchers.Delete(id)
		return nil
	}

	return fmt.Errorf("no file watcher exists with ID %s", id)
}

// RefreshDiagnostics implements project.Client.
func (s *Server) RefreshDiagnostics(ctx context.Context) error {
	if !s.clientCapabilities.Workspace.Diagnostics.RefreshSupport {
		return nil
	}

	if _, err := sendClientRequest(ctx, s, lsproto.WorkspaceDiagnosticRefreshInfo, nil); err != nil {
		return fmt.Errorf("failed to refresh diagnostics: %w", err)
	}

	return nil
}

// PublishDiagnostics implements project.Client.
func (s *Server) PublishDiagnostics(ctx context.Context, params *lsproto.PublishDiagnosticsParams) error {
	return sendNotification(s, lsproto.TextDocumentPublishDiagnosticsInfo, params)
}

func (s *Server) RefreshInlayHints(ctx context.Context) error {
	if !s.clientCapabilities.Workspace.InlayHint.RefreshSupport {
		return nil
	}

	if _, err := sendClientRequest(ctx, s, lsproto.WorkspaceInlayHintRefreshInfo, nil); err != nil {
		return fmt.Errorf("failed to refresh inlay hints: %w", err)
	}
	return nil
}

func (s *Server) RefreshCodeLens(ctx context.Context) error {
	if !s.clientCapabilities.Workspace.CodeLens.RefreshSupport {
		return nil
	}

	if _, err := sendClientRequest(ctx, s, lsproto.WorkspaceCodeLensRefreshInfo, nil); err != nil {
		return fmt.Errorf("failed to refresh code lens: %w", err)
	}
	return nil
}

func (s *Server) RequestConfiguration(ctx context.Context) (*lsutil.UserConfig, error) {
	caps := lsproto.GetClientCapabilities(ctx)
	if !caps.Workspace.Configuration {
		if s.initializeParams != nil && s.initializeParams.InitializationOptions != nil && s.initializeParams.InitializationOptions.UserPreferences != nil {
			s.logger.Logf(
				"received formatting options from initialization: %T\n%+v",
				*s.initializeParams.InitializationOptions.UserPreferences,
				*s.initializeParams.InitializationOptions.UserPreferences,
			)
			// Any options received via initializationOptions will be used for both `js` and `ts` options
			if config, ok := (*s.initializeParams.InitializationOptions.UserPreferences).(map[string]any); ok {
				return lsutil.NewUserConfig(lsutil.NewDefaultUserPreferences().ParseWorker(config)), nil
			}
		}
		// if no configuration request capability, return default config
		return lsutil.NewUserConfig(nil), nil
	}
	configs, err := sendClientRequest(ctx, s, lsproto.WorkspaceConfigurationInfo, &lsproto.ConfigurationParams{
		Items: []*lsproto.ConfigurationItem{
			{
				Section: new("js/ts"),
			},
			{
				Section: new("typescript"),
			},
			{
				Section: new("javascript"),
			},
			{
				Section: new("editor"),
			},
		},
	})
	if err != nil {
		return &lsutil.UserConfig{}, fmt.Errorf("configure request failed: %w", err)
	}
	configMap := map[string]any{}
	for i, config := range configs {
		switch i {
		case 0:
			configMap["js/ts"] = config
		case 1:
			configMap["typescript"] = config
		case 2:
			configMap["javascript"] = config
		case 3:
			configMap["editor"] = config
		}
	}
	s.logger.Logf(
		"received options from workspace/configuration request:\njs/ts: %+v\n\ntypescript: %+v\n\njavascript: %+v\n\neditor: %+v\n",
		configMap["js/ts"],
		configMap["typescript"],
		configMap["javascript"],
		configMap["editor"],
	)
	return lsutil.ParseNewUserConfig(configMap), nil
}

func (s *Server) Run(ctx context.Context) error {
	g, ctx := errgroup.WithContext(ctx)
	s.backgroundCtx = ctx
	g.Go(func() error { return s.dispatchLoop(ctx) })
	g.Go(func() error { return s.writeLoop(ctx) })

	// Don't run readLoop in the group, as it blocks on stdin read and cannot be cancelled.
	readLoopErr := make(chan error, 1)
	g.Go(func() error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-readLoopErr:
			return err
		}
	})
	go func() { readLoopErr <- s.readLoop(ctx) }()

	if err := g.Wait(); err != nil && !errors.Is(err, io.EOF) && ctx.Err() != nil {
		return err
	}
	return nil
}

func (s *Server) readLoop(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		msg, err := s.read()
		if err != nil {
			if errors.Is(err, lsproto.ErrorCodeInvalidRequest) || errors.Is(err, lsproto.ErrorCodeInvalidParams) {
				var id *jsonrpc.ID
				if errors.Is(err, lsproto.ErrorCodeInvalidParams) {
					if msg != nil && msg.Kind == jsonrpc.MessageKindRequest {
						id = msg.AsRequest().ID
					}
				}
				if err := s.sendError(id, err); err != nil {
					return err
				}
				continue
			}
			return err
		}

		if s.initializeParams == nil && msg.Kind == jsonrpc.MessageKindRequest {
			req := msg.AsRequest()
			if req.Method == lsproto.MethodInitialize {
				resp, err := s.handleInitialize(ctx, req.Params.(*lsproto.InitializeParams), req)
				if err != nil {
					return err
				}
				if err := s.sendResult(req.ID, resp); err != nil {
					return err
				}
			} else {
				if err := s.sendError(req.ID, lsproto.ErrorCodeServerNotInitialized); err != nil {
					return err
				}
			}
			continue
		}

		if msg.Kind == jsonrpc.MessageKindResponse {
			resp := msg.AsResponse()
			s.pendingServerRequestsMu.Lock()
			if respChan, ok := s.pendingServerRequests[*resp.ID]; ok {
				respChan <- resp
				close(respChan)
				delete(s.pendingServerRequests, *resp.ID)
			}
			s.pendingServerRequestsMu.Unlock()
		} else {
			req := msg.AsRequest()
			if req.Method == lsproto.MethodCancelRequest {
				s.cancelRequest(req.Params.(*lsproto.CancelParams).Id)
			} else {
				s.requestQueue <- req
			}
		}
	}
}

func (s *Server) cancelRequest(rawID lsproto.IntegerOrString) {
	id := lsproto.NewID(rawID)
	s.pendingClientRequestsMu.Lock()
	defer s.pendingClientRequestsMu.Unlock()
	if pendingReq, ok := s.pendingClientRequests[*id]; ok {
		pendingReq.cancel()
		delete(s.pendingClientRequests, *id)
	}
}

func (s *Server) read() (*lsproto.Message, error) {
	return s.r.Read()
}

func (s *Server) dispatchLoop(ctx context.Context) error {
	ctx, lspExit := context.WithCancelCause(ctx)
	defer lspExit(nil)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case req := <-s.requestQueue:
			requestCtx := locale.WithLocale(ctx, s.locale)
			if req.ID != nil {
				var cancel context.CancelFunc
				requestCtx, cancel = context.WithCancel(core.WithRequestID(requestCtx, req.ID.String()))
				s.pendingClientRequestsMu.Lock()
				s.pendingClientRequests[*req.ID] = pendingClientRequest{
					req:    req,
					cancel: cancel,
				}
				s.pendingClientRequestsMu.Unlock()
			}

			handleError := func(err error) {
				if errors.Is(err, context.Canceled) {
					if err := s.sendError(req.ID, lsproto.ErrorCodeRequestCancelled); err != nil {
						lspExit(err)
					}
				} else if errors.Is(err, io.EOF) {
					lspExit(nil)
				} else {
					if err := s.sendError(req.ID, err); err != nil {
						lspExit(err)
					}
				}
			}

			removeRequest := func() {
				if req.ID != nil {
					s.pendingClientRequestsMu.Lock()
					defer s.pendingClientRequestsMu.Unlock()
					delete(s.pendingClientRequests, *req.ID)
				}
			}

			if doAsyncWork, err := s.handleRequestOrNotification(requestCtx, req); err != nil {
				handleError(err)
				removeRequest()
			} else if doAsyncWork != nil {
				go func() {
					if lsError := doAsyncWork(); lsError != nil {
						handleError(lsError)
					}
					removeRequest()
				}()
			} else {
				removeRequest()
			}
		}
	}
}

func (s *Server) writeLoop(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg := <-s.outgoingQueue:
			if err := s.w.Write(msg); err != nil {
				return fmt.Errorf("failed to write message: %w", err)
			}
		}
	}
}

func sendClientRequest[Req, Resp any](ctx context.Context, s *Server, info lsproto.RequestInfo[Req, Resp], params Req) (Resp, error) {
	id := jsonrpc.NewIDString(fmt.Sprintf("ts%d", s.clientSeq.Add(1)))
	req := info.NewRequestMessage(id, params)

	responseChan := make(chan *lsproto.ResponseMessage, 1)
	s.pendingServerRequestsMu.Lock()
	s.pendingServerRequests[*id] = responseChan
	s.pendingServerRequestsMu.Unlock()

	defer func() {
		s.pendingServerRequestsMu.Lock()
		defer s.pendingServerRequestsMu.Unlock()
		if respChan, ok := s.pendingServerRequests[*id]; ok {
			close(respChan)
			delete(s.pendingServerRequests, *id)
		}
	}()

	if err := s.send(req.Message()); err != nil {
		return *new(Resp), err
	}

	select {
	case <-ctx.Done():
		return *new(Resp), ctx.Err()
	case resp := <-responseChan:
		if resp.Error != nil {
			return *new(Resp), fmt.Errorf("request failed: %s", resp.Error.String())
		}
		return info.UnmarshalResult(resp.Result)
	}
}

// sendRequestSync sends a request to the client and waits for the response, unmarshaling into result
func (s *Server) sendRequestSync(ctx context.Context, method lsproto.Method, params any, result any) error {
	id := jsonrpc.NewIDString(fmt.Sprintf("ts%d", s.clientSeq.Add(1)))
	req := &lsproto.RequestMessage{
		ID:     id,
		Method: method,
		Params: params,
	}

	responseChan := make(chan *lsproto.ResponseMessage, 1)
	s.pendingServerRequestsMu.Lock()
	s.pendingServerRequests[*id] = responseChan
	s.pendingServerRequestsMu.Unlock()

	s.outgoingQueue <- req.Message()

	select {
	case <-ctx.Done():
		s.pendingServerRequestsMu.Lock()
		defer s.pendingServerRequestsMu.Unlock()
		if respChan, ok := s.pendingServerRequests[*id]; ok {
			close(respChan)
			delete(s.pendingServerRequests, *id)
		}
		return ctx.Err()
	case resp := <-responseChan:
		if resp.Error != nil {
			return fmt.Errorf("request failed: %s", resp.Error.String())
		}
		if result != nil && resp.Result != nil {
			raw, ok := resp.Result.(jsontext.Value)
			if !ok {
				return fmt.Errorf("expected jsontext.Value, got %T", resp.Result)
			}
			return json.Unmarshal(raw, result)
		}
		return nil
	}
}

func (s *Server) sendResult(id *jsonrpc.ID, result any) error {
	return s.sendResponse(&lsproto.ResponseMessage{
		ID:     id,
		Result: result,
	})
}

func (s *Server) sendError(id *jsonrpc.ID, err error) error {
	// Do not send error response for notifications,
	// except for parse errors which may occur before determining if the message is a request or notification.
	if id == nil && !errors.Is(err, lsproto.ErrorCodeInvalidRequest) {
		s.logger.Errorf("error handling notification: %s", err)
		return nil
	}
	code := lsproto.ErrorCodeInternalError
	if errCode, ok := errors.AsType[lsproto.ErrorCode](err); ok {
		code = errCode
	}
	// TODO(jakebailey): error data
	return s.sendResponse(&lsproto.ResponseMessage{
		ID: id,
		Error: &jsonrpc.ResponseError{
			Code:    int32(code),
			Message: err.Error(),
		},
	})
}

func sendNotification[Params any](s *Server, info lsproto.NotificationInfo[Params], params Params) error {
	return s.send(info.NewNotificationMessage(params).Message())
}

func (s *Server) sendResponse(resp *lsproto.ResponseMessage) error {
	return s.send(resp.Message())
}

// send writes a message to the outgoing queue, respecting context cancellation.
func (s *Server) send(msg *lsproto.Message) error {
	select {
	case s.outgoingQueue <- msg:
		return nil
	case <-s.backgroundCtx.Done():
		return s.backgroundCtx.Err()
	}
}

// handleRequestOrNotification looks up the handler for the given request or notification, executes its synchronous work
// and returns any asynchronous work as a function to be executed by the caller.
func (s *Server) handleRequestOrNotification(ctx context.Context, req *lsproto.RequestMessage) (func() error, error) {
	ctx = lsproto.WithClientCapabilities(ctx, &s.clientCapabilities)

	if handler := handlers()[req.Method]; handler != nil {
		start := time.Now()
		doAsyncWork, err := handler(s, ctx, req)
		idStr := ""
		if req.ID != nil {
			idStr = " (" + req.ID.String() + ")"
		}
		if err != nil {
			s.logger.Error("error handling method '", req.Method, "'", idStr, ": ", err)
			return nil, err
		}
		if doAsyncWork != nil {
			return func() error {
				// note: ctx.Err() has to be checked in the async work to allow async handlers to cleanup resources correctly
				asyncWorkErr := doAsyncWork()
				s.logger.Info(core.IfElse(asyncWorkErr != nil, "error handling method '", "handled method '"), req.Method, "'", idStr, " in ", time.Since(start))
				return asyncWorkErr
			}, nil
		}
		s.logger.Info("handled method '", req.Method, "'", idStr, " in ", time.Since(start))
		return nil, nil
	}
	s.logger.Warn("unknown method '", req.Method, "'")
	if req.ID != nil {
		return nil, s.sendError(req.ID, lsproto.ErrorCodeInvalidRequest)
	}
	return nil, nil
}

// handlerMap maps LSP method to a handler function. The handler function executes any work that must be done synchronously
// before other requests/notifications can be processed, and returns any additional work as a function to be executed
// asynchronously after the synchronous work is complete.
type handlerMap map[lsproto.Method]func(*Server, context.Context, *lsproto.RequestMessage) (func() error, error)

var handlers = sync.OnceValue(func() handlerMap {
	handlers := make(handlerMap)

	registerRequestHandler(handlers, lsproto.InitializeInfo, (*Server).handleInitialize)
	registerNotificationHandler(handlers, lsproto.InitializedInfo, (*Server).handleInitialized)
	registerRequestHandler(handlers, lsproto.ShutdownInfo, (*Server).handleShutdown)
	registerNotificationHandler(handlers, lsproto.ExitInfo, (*Server).handleExit)

	registerNotificationHandler(handlers, lsproto.WorkspaceDidChangeConfigurationInfo, (*Server).handleDidChangeWorkspaceConfiguration)
	registerNotificationHandler(handlers, lsproto.TextDocumentDidOpenInfo, (*Server).handleDidOpen)
	registerNotificationHandler(handlers, lsproto.TextDocumentDidChangeInfo, (*Server).handleDidChange)
	registerNotificationHandler(handlers, lsproto.TextDocumentDidSaveInfo, (*Server).handleDidSave)
	registerNotificationHandler(handlers, lsproto.TextDocumentDidCloseInfo, (*Server).handleDidClose)
	registerNotificationHandler(handlers, lsproto.WorkspaceDidChangeWatchedFilesInfo, (*Server).handleDidChangeWatchedFiles)
	registerNotificationHandler(handlers, lsproto.SetTraceInfo, (*Server).handleSetTrace)

	registerLanguageServiceDocumentRequestHandler(handlers, lsproto.TextDocumentDiagnosticInfo, (*Server).handleDocumentDiagnostic)
	registerLanguageServiceDocumentRequestHandler(handlers, lsproto.TextDocumentHoverInfo, (*Server).handleHover)
	registerLanguageServiceDocumentRequestHandler(handlers, lsproto.TextDocumentDefinitionInfo, (*Server).handleDefinition)
	registerLanguageServiceDocumentRequestHandler(handlers, lsproto.TextDocumentTypeDefinitionInfo, (*Server).handleTypeDefinition)
	registerLanguageServiceDocumentRequestHandler(handlers, lsproto.TextDocumentSignatureHelpInfo, (*Server).handleSignatureHelp)
	registerLanguageServiceDocumentRequestHandler(handlers, lsproto.TextDocumentFormattingInfo, (*Server).handleDocumentFormat)
	registerLanguageServiceDocumentRequestHandler(handlers, lsproto.TextDocumentRangeFormattingInfo, (*Server).handleDocumentRangeFormat)
	registerLanguageServiceDocumentRequestHandler(handlers, lsproto.TextDocumentOnTypeFormattingInfo, (*Server).handleDocumentOnTypeFormat)
	registerLanguageServiceDocumentRequestHandler(handlers, lsproto.TextDocumentDocumentSymbolInfo, (*Server).handleDocumentSymbol)
	registerLanguageServiceDocumentRequestHandler(handlers, lsproto.TextDocumentDocumentHighlightInfo, (*Server).handleDocumentHighlight)
	registerLanguageServiceDocumentRequestHandler(handlers, lsproto.TextDocumentSelectionRangeInfo, (*Server).handleSelectionRange)
	registerLanguageServiceDocumentRequestHandler(handlers, lsproto.TextDocumentInlayHintInfo, (*Server).handleInlayHint)
	registerLanguageServiceDocumentRequestHandler(handlers, lsproto.TextDocumentCodeLensInfo, (*Server).handleCodeLens)
	registerLanguageServiceDocumentRequestHandler(handlers, lsproto.TextDocumentCodeActionInfo, (*Server).handleCodeAction)
	registerLanguageServiceDocumentRequestHandler(handlers, lsproto.TextDocumentPrepareCallHierarchyInfo, (*Server).handlePrepareCallHierarchy)
	registerLanguageServiceDocumentRequestHandler(handlers, lsproto.TextDocumentFoldingRangeInfo, (*Server).handleFoldingRange)

	registerLanguageServiceWithAutoImportsRequestHandler(handlers, lsproto.TextDocumentCompletionInfo, (*Server).handleCompletion)
	registerLanguageServiceWithAutoImportsRequestHandler(handlers, lsproto.TextDocumentCodeActionInfo, (*Server).handleCodeAction)

	registerLanguageServiceDocumentRequestHandler(handlers, lsproto.CustomTextDocumentClosingTagCompletionInfo, (*Server).handleClosingTagCompletion)

	registerMultiProjectReferenceRequestHandler(handlers, lsproto.TextDocumentReferencesInfo, (*ls.LanguageService).ProvideReferences)
	registerMultiProjectReferenceRequestHandler(handlers, lsproto.TextDocumentRenameInfo, (*ls.LanguageService).ProvideRename)
	registerMultiProjectReferenceRequestHandler(handlers, lsproto.TextDocumentImplementationInfo, (*ls.LanguageService).ProvideImplementations)

	registerRequestHandler(handlers, lsproto.CallHierarchyIncomingCallsInfo, (*Server).handleCallHierarchyIncomingCalls)
	registerRequestHandler(handlers, lsproto.CallHierarchyOutgoingCallsInfo, (*Server).handleCallHierarchyOutgoingCalls)

	registerRequestHandler(handlers, lsproto.WorkspaceSymbolInfo, (*Server).handleWorkspaceSymbol)
	registerRequestHandler(handlers, lsproto.CompletionItemResolveInfo, (*Server).handleCompletionItemResolve)
	registerRequestHandler(handlers, lsproto.CodeLensResolveInfo, (*Server).handleCodeLensResolve)

	registerRequestHandler(handlers, lsproto.DenoRequestInfo, (*Server).handleDenoRequest)

	// Developer/debugging commands
	registerRequestHandler(handlers, lsproto.CustomRunGCInfo, (*Server).handleRunGC)
	registerRequestHandler(handlers, lsproto.CustomSaveHeapProfileInfo, (*Server).handleSaveHeapProfile)
	registerRequestHandler(handlers, lsproto.CustomSaveAllocProfileInfo, (*Server).handleSaveAllocProfile)
	registerRequestHandler(handlers, lsproto.CustomStartCPUProfileInfo, (*Server).handleStartCPUProfile)
	registerRequestHandler(handlers, lsproto.CustomStopCPUProfileInfo, (*Server).handleStopCPUProfile)

	registerRequestHandler(handlers, lsproto.CustomInitializeAPISessionInfo, (*Server).handleInitializeAPISession)
	return handlers
})

func registerNotificationHandler[Req any](handlers handlerMap, info lsproto.NotificationInfo[Req], fn func(*Server, context.Context, Req) error) {
	handlers[info.Method] = func(s *Server, ctx context.Context, req *lsproto.RequestMessage) (func() error, error) {
		if s.session == nil && req.Method != lsproto.MethodInitialized {
			return nil, lsproto.ErrorCodeServerNotInitialized
		}

		var params Req
		// Ignore empty params; all generated params are either pointers or any.
		if req.Params != nil {
			params = req.Params.(Req)
		}
		if err := fn(s, ctx, params); err != nil {
			return nil, err
		}
		return nil, ctx.Err()
	}
}

func registerRequestHandler[Req, Resp any](
	handlers handlerMap,
	info lsproto.RequestInfo[Req, Resp],
	fn func(*Server, context.Context, Req, *lsproto.RequestMessage) (Resp, error),
) {
	handlers[info.Method] = func(s *Server, ctx context.Context, req *lsproto.RequestMessage) (func() error, error) {
		if s.session == nil && req.Method != lsproto.MethodInitialize {
			return nil, lsproto.ErrorCodeServerNotInitialized
		}

		var params Req
		// Ignore empty params.
		if req.Params != nil {
			params = req.Params.(Req)
		}
		resp, err := fn(s, ctx, params, req)
		if err != nil {
			return nil, err
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, s.sendResult(req.ID, resp)
	}
}

func registerLanguageServiceDocumentRequestHandler[Req lsproto.HasTextDocumentURI, Resp any](handlers handlerMap, info lsproto.RequestInfo[Req, Resp], fn func(*Server, context.Context, *ls.LanguageService, Req) (Resp, error)) {
	handlers[info.Method] = func(s *Server, ctx context.Context, req *lsproto.RequestMessage) (func() error, error) {
		var params Req
		// Ignore empty params.
		if req.Params != nil {
			params = req.Params.(Req)
		}
		ls, err := s.session.GetLanguageService(ctx, params.TextDocumentURI())
		if err != nil {
			return nil, err
		}
		return func() error {
			defer s.recover(req)
			resp, lsErr := fn(s, ctx, ls, params)
			if lsErr != nil {
				return lsErr
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return s.sendResult(req.ID, resp)
		}, nil
	}
}

func registerLanguageServiceWithAutoImportsRequestHandler[Req lsproto.HasTextDocumentURI, Resp any](handlers handlerMap, info lsproto.RequestInfo[Req, Resp], fn func(*Server, context.Context, *ls.LanguageService, Req) (Resp, error)) {
	handlers[info.Method] = func(s *Server, ctx context.Context, req *lsproto.RequestMessage) (func() error, error) {
		var params Req
		// Ignore empty params.
		if req.Params != nil {
			params = req.Params.(Req)
		}
		languageService, snapshot, releaseSnapshot, err := s.session.GetLanguageServiceAndSnapshot(ctx, params.TextDocumentURI())
		if err != nil {
			return nil, err
		}
		return func() error {
			defer releaseSnapshot()
			defer s.recover(req)
			resp, lsErr := fn(s, ctx, languageService, params)
			if errors.Is(lsErr, ls.ErrNeedsAutoImports) {
				var releaseAutoImports func()
				languageService, releaseAutoImports, lsErr = s.session.GetLanguageServiceWithAutoImports(ctx, snapshot, params.TextDocumentURI())
				if lsErr != nil {
					return lsErr
				}
				defer releaseAutoImports()
				if ctx.Err() != nil {
					return ctx.Err()
				}
				resp, lsErr = fn(s, ctx, languageService, params)
				if errors.Is(lsErr, ls.ErrNeedsAutoImports) {
					panic(info.Method + " returned ErrNeedsAutoImports even after enabling auto imports")
				}
			}
			if lsErr != nil {
				return lsErr
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return s.sendResult(req.ID, resp)
		}, nil
	}
}

func registerMultiProjectReferenceRequestHandler[Req lsproto.HasTextDocumentPosition, Resp any](
	handlers handlerMap,
	info lsproto.RequestInfo[Req, Resp],
	fn func(*ls.LanguageService, context.Context, Req, ls.CrossProjectOrchestrator) (Resp, error),
) {
	handlers[info.Method] = func(s *Server, ctx context.Context, req *lsproto.RequestMessage) (func() error, error) {
		var params Req
		// Ignore empty params.
		if req.Params != nil {
			params = req.Params.(Req)
		}
		// !!! sheetal: multiple projects that contain the file through symlinks
		defaultLs, orchestrator, err := s.getLanguageServiceAndCrossProjectOrchestrator(ctx, params.TextDocumentURI(), req)
		if err != nil {
			return nil, err
		}
		return func() error {
			defer s.recover(req)
			resp, lsErr := fn(defaultLs, ctx, params, orchestrator)
			if lsErr != nil {
				return lsErr
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return s.sendResult(req.ID, resp)
		}, nil
	}
}

type crossProjectOrchestrator struct {
	server         *Server
	req            *lsproto.RequestMessage
	defaultProject *project.Project
	allProjects    []ls.Project
}

var _ ls.CrossProjectOrchestrator = (*crossProjectOrchestrator)(nil)

func (c *crossProjectOrchestrator) GetDefaultProject() ls.Project {
	return c.defaultProject
}

func (c *crossProjectOrchestrator) GetAllProjectsForInitialRequest() []ls.Project {
	return c.allProjects
}

func (c *crossProjectOrchestrator) GetLanguageServiceForProjectWithFile(ctx context.Context, p ls.Project, uri lsproto.DocumentUri) *ls.LanguageService {
	return c.server.session.GetLanguageServiceForProjectWithFile(ctx, p.(*project.Project), uri)
}

func (c *crossProjectOrchestrator) GetProjectsForFile(ctx context.Context, uri lsproto.DocumentUri) ([]ls.Project, error) {
	return c.server.session.GetProjectsForFile(ctx, uri)
}

func (c *crossProjectOrchestrator) GetProjectsLoadingProjectTree(ctx context.Context, requestedProjectTrees *collections.Set[tspath.Path]) iter.Seq[ls.Project] {
	return func(yield func(ls.Project) bool) {
		for _, p := range c.server.session.GetSnapshotLoadingProjectTree(ctx, requestedProjectTrees).ProjectCollection.Projects() {
			if !yield(p) {
				return
			}
		}
	}
}

func (s *Server) getLanguageServiceAndCrossProjectOrchestrator(ctx context.Context, uri lsproto.DocumentUri, req *lsproto.RequestMessage) (*ls.LanguageService, ls.CrossProjectOrchestrator, error) {
	defaultProject, defaultLs, allProjects, err := s.session.GetLanguageServiceAndProjectsForFile(ctx, uri)
	var orchestrator ls.CrossProjectOrchestrator
	if err == nil {
		orchestrator = &crossProjectOrchestrator{s, req, defaultProject, allProjects}
	}
	return defaultLs, orchestrator, err
}

func (s *Server) recover(req *lsproto.RequestMessage) {
	if r := recover(); r != nil {
		stack := debug.Stack()
		s.logger.Errorf("panic handling request %s: %v\n%s", req.Method, r, string(stack))
		if req.ID != nil {
			err := s.sendError(req.ID, fmt.Errorf("%w: panic handling request %s: %v", lsproto.ErrorCodeInternalError, req.Method, r))
			if err != nil {
				return
			}

			_ = sendNotification(s, lsproto.TelemetryEventInfo, lsproto.TelemetryEvent{
				RequestFailureTelemetryEvent: &lsproto.RequestFailureTelemetryEvent{
					Properties: &lsproto.RequestFailureTelemetryProperties{
						ErrorCode:     lsproto.ErrorCodeInternalError.String(),
						RequestMethod: strings.ReplaceAll(string(req.Method), "/", "."),
						Stack:         sanitizeStackTrace(string(stack)),
					},
				},
			})
		} else {
			s.logger.Error("unhandled panic in notification", req.Method, r)
		}
	}
}

type DenoLanguageServiceProject struct {
	programEntry *DenoProgramEntry
}

func (p DenoLanguageServiceProject) Id() tspath.Path {
	return p.programEntry.projectPath
}

func (p DenoLanguageServiceProject) GetProgram() *compiler.Program {
	return p.programEntry.program
}

func (p DenoLanguageServiceProject) HasFile(fileName string) bool {
	path := tspath.ToPath(fileName, p.programEntry.program.GetCurrentDirectory(), p.programEntry.program.Host().FS().UseCaseSensitiveFileNames())
	return p.programEntry.program.GetSourceFileByPath(path) != nil
}

type DenoCrossProjectOrchestrator struct {
	server              *Server
	ctx                 context.Context
	defaultProgramEntry *DenoProgramEntry
	defaultHost         *DenoLanguageServiceHost
	initialUri          lsproto.DocumentUri
}

func (o *DenoCrossProjectOrchestrator) GetDefaultProject() ls.Project {
	return DenoLanguageServiceProject{programEntry: o.defaultProgramEntry}
}

func (o *DenoCrossProjectOrchestrator) GetAllProjectsForInitialRequest() []ls.Project {
	fileName := o.initialUri.FileName()
	entries := o.server.deno.programEntries
	var projects []ls.Project
	for _, pe := range entries.byCompilerOptionsKey {
		if (DenoLanguageServiceProject{programEntry: pe}).HasFile(fileName) {
			projects = append(projects, DenoLanguageServiceProject{programEntry: pe})
		}
	}
	for _, pe := range entries.byNotebookUri {
		if (DenoLanguageServiceProject{programEntry: pe}).HasFile(fileName) {
			projects = append(projects, DenoLanguageServiceProject{programEntry: pe})
		}
	}
	return projects
}

func (o *DenoCrossProjectOrchestrator) GetLanguageServiceForProjectWithFile(ctx context.Context, project ls.Project, uri lsproto.DocumentUri) *ls.LanguageService {
	denoProject, ok := project.(DenoLanguageServiceProject)
	if !ok {
		return nil
	}
	pe := denoProject.programEntry
	host := NewDenoLanguageServiceHost(pe.vfs, pe.userPreferences, pe.autoImportRegistry)
	return ls.NewLanguageService(pe.projectPath, pe.program, host, uri.FileName())
}

func (o *DenoCrossProjectOrchestrator) GetProjectsForFile(ctx context.Context, uri lsproto.DocumentUri) ([]ls.Project, error) {
	fileName := uri.FileName()
	var projects []ls.Project
	for _, pe := range o.server.deno.programEntries.byCompilerOptionsKey {
		project := DenoLanguageServiceProject{programEntry: pe}
		if project.HasFile(fileName) {
			projects = append(projects, project)
		}
	}
	for _, pe := range o.server.deno.programEntries.byNotebookUri {
		project := DenoLanguageServiceProject{programEntry: pe}
		if project.HasFile(fileName) {
			projects = append(projects, project)
		}
	}
	if len(projects) == 0 {
		// Fall back to default project if no project contains the file
		return []ls.Project{o.GetDefaultProject()}, nil
	}
	return projects, nil
}

func (o *DenoCrossProjectOrchestrator) GetProjectsLoadingProjectTree(ctx context.Context, requestedProjectTrees *collections.Set[tspath.Path]) iter.Seq[ls.Project] {
	return func(yield func(ls.Project) bool) {
		for _, pe := range o.server.deno.programEntries.byCompilerOptionsKey {
			if requestedProjectTrees != nil && !requestedProjectTrees.Has(pe.projectPath) {
				continue
			}
			if !yield(DenoLanguageServiceProject{programEntry: pe}) {
				return
			}
		}
		for _, pe := range o.server.deno.programEntries.byNotebookUri {
			if requestedProjectTrees != nil && !requestedProjectTrees.Has(pe.projectPath) {
				continue
			}
			if !yield(DenoLanguageServiceProject{programEntry: pe}) {
				return
			}
		}
	}
}

func (s *Server) handleDenoRequest(ctx context.Context, params *lsproto.DenoRequestParams, _ *lsproto.RequestMessage) (any, error) {
	if params.ApplyWorkspaceChange != nil {
		if params.ApplyWorkspaceChange.NewConfiguration != nil {
			// Clear document cache on new configuration
			s.deno.documentCacheMu.Lock()
			s.deno.documentCache = make(map[lsproto.DocumentUri]*lsproto.DenoDocumentData)
			s.deno.documentCacheMu.Unlock()

			byCompilerOptionsKey := make(map[string]*DenoProgramEntry, len(params.ApplyWorkspaceChange.NewConfiguration.ByCompilerOptionsKey))
			byNotebookUri := make(map[string]*DenoProgramEntry, len(params.ApplyWorkspaceChange.NewConfiguration.ByNotebookUri))
			for key, projectConfig := range params.ApplyWorkspaceChange.NewConfiguration.ByCompilerOptionsKey {
				entry, err := s.createDenoProgramEntry(ctx, key, nil, projectConfig)
				if err != nil {
					return nil, fmt.Errorf("Failed to create program entry for key %s: %w", key, err)
				}
				byCompilerOptionsKey[key] = entry
			}
			for notebookUri, projectConfig := range params.ApplyWorkspaceChange.NewConfiguration.ByNotebookUri {
				entry, err := s.createDenoProgramEntry(ctx, projectConfig.CompilerOptionsKey, &notebookUri, projectConfig)
				if err != nil {
					return nil, fmt.Errorf("Failed to create program entry for notebook %s: %w", notebookUri, err)
				}
				byNotebookUri[notebookUri] = entry
			}
			s.deno.programEntries = DenoProgramEntries{
				byCompilerOptionsKey: byCompilerOptionsKey,
				byNotebookUri:        byNotebookUri,
			}
		} else {
			// Handle file changes by updating affected programs
			for _, fileChange := range params.ApplyWorkspaceChange.FileChanges {
				// Clear document cache entry for changed file
				s.deno.documentCacheMu.Lock()
				normalizedUri := lsconv.FileNameToDocumentURI(tspath.NormalizePath(fileChange.Uri.FileName()))
				delete(s.deno.documentCache, normalizedUri)
				s.deno.documentCacheMu.Unlock()

				filePath := tspath.ToPath(fileChange.Uri.FileName(), s.cwd, true)
				updateProgram := func(pe *DenoProgramEntry) {
					if pe.program.GetSourceFileByPath(filePath) != nil {
						wrappedFS := bundled.WrapFS(pe.vfs)
						baseHost := compiler.NewCompilerHost(
							s.cwd,
							wrappedFS,
							bundled.LibPath(),
							nil,
							nil,
						)
						// Wrap the host to intercept resolution
						newHost := &denoCompilerHost{
							inner:              baseHost,
							server:             s,
							vfs:                pe.vfs,
							compilerOptionsKey: pe.compilerOptionsKey,
							notebookUri:        pe.notebookUri,
						}
						newProgram, _ := pe.program.UpdateProgram(filePath, newHost)
						pe.program = newProgram
					}
				}
				for _, pe := range s.deno.programEntries.byCompilerOptionsKey {
					updateProgram(pe)
				}
				for _, pe := range s.deno.programEntries.byNotebookUri {
					updateProgram(pe)
				}
			}
		}
		workspaceInfo := lsproto.DenoWorkspaceInfo{
			CompilerOptionsKeysForAssets: make(map[lsproto.DocumentUri]string),
		}
		for compilerOptionsKey, pe := range s.deno.programEntries.byCompilerOptionsKey {
			for _, file := range pe.program.SourceFiles() {
				if strings.HasPrefix(file.FileName(), "asset:///") {
					uri := lsconv.FileNameToDocumentURI(file.FileName())
					if _, ok := workspaceInfo.CompilerOptionsKeysForAssets[uri]; !ok {
						workspaceInfo.CompilerOptionsKeysForAssets[uri] = compilerOptionsKey
					}
				}
			}
		}
		return workspaceInfo, nil
	} else if params.LanguageServiceMethod != nil {
		req := params.LanguageServiceMethod
		pe, languageService, err := s.getDenoLanguageService(req.CompilerOptionsKey, req.NotebookUri)
		if err != nil {
			return nil, err
		}
		unmarshalArg := func(arg any, target any) {
			bytes, _ := json.Marshal(arg)
			json.Unmarshal(bytes, target)
		}
		switch req.Name {
		case "ProvideDiagnostics":
			var p0 lsproto.DocumentUri
			unmarshalArg(req.Args[0], &p0)
			return languageService.ProvideDiagnostics(ctx, p0)
		case "ProvideReferences":
			var p0 lsproto.ReferenceParams
			unmarshalArg(req.Args[0], &p0)
			orchestrator := s.getDenoCrossProjectOrchestrator(ctx, pe, p0.TextDocument.Uri)
			return languageService.ProvideReferences(ctx, &p0, orchestrator)
		case "ProvideCodeLenses":
			var p0 lsproto.DocumentUri
			unmarshalArg(req.Args[0], &p0)
			return languageService.ProvideCodeLenses(ctx, p0)
		case "ProvideDocumentSymbols":
			var p0 lsproto.DocumentUri
			unmarshalArg(req.Args[0], &p0)
			return languageService.ProvideDocumentSymbols(ctx, p0)
		case "ProvideHover":
			var p0 lsproto.DocumentUri
			unmarshalArg(req.Args[0], &p0)
			var p1 lsproto.Position
			unmarshalArg(req.Args[1], &p1)
			return languageService.ProvideHover(ctx, p0, p1)
		case "ProvideCodeActions":
			var p0 lsproto.CodeActionParams
			unmarshalArg(req.Args[0], &p0)
			return languageService.ProvideCodeActions(ctx, &p0)
		case "ProvideDocumentHighlights":
			var p0 lsproto.DocumentUri
			unmarshalArg(req.Args[0], &p0)
			var p1 lsproto.Position
			unmarshalArg(req.Args[1], &p1)
			return languageService.ProvideDocumentHighlights(ctx, p0, p1)
		case "ProvideDefinition":
			var p0 lsproto.DocumentUri
			unmarshalArg(req.Args[0], &p0)
			var p1 lsproto.Position
			unmarshalArg(req.Args[1], &p1)
			return languageService.ProvideDefinition(ctx, p0, p1)
		case "ProvideTypeDefinition":
			var p0 lsproto.DocumentUri
			unmarshalArg(req.Args[0], &p0)
			var p1 lsproto.Position
			unmarshalArg(req.Args[1], &p1)
			return languageService.ProvideTypeDefinition(ctx, p0, p1)
		case "ProvideCompletion":
			var p0 lsproto.DocumentUri
			unmarshalArg(req.Args[0], &p0)
			var p1 lsproto.Position
			unmarshalArg(req.Args[1], &p1)
			var p2 lsproto.CompletionContext
			unmarshalArg(req.Args[2], &p2)
			return languageService.ProvideCompletion(ctx, p0, p1, &p2)
		case "ResolveCompletionItem":
			var p0 lsproto.CompletionItem
			unmarshalArg(req.Args[0], &p0)
			return languageService.ResolveCompletionItem(ctx, &p0, p0.Data)
		case "ProvideImplementations":
			var p0 lsproto.ImplementationParams
			unmarshalArg(req.Args[0], &p0)
			orchestrator := s.getDenoCrossProjectOrchestrator(ctx, pe, p0.TextDocument.Uri)
			return languageService.ProvideImplementations(ctx, &p0, orchestrator)
		case "ProvideFoldingRange":
			var p0 lsproto.DocumentUri
			unmarshalArg(req.Args[0], &p0)
			return languageService.ProvideFoldingRange(ctx, p0)
		case "ProvideCallHierarchyIncomingCalls":
			var p0 lsproto.CallHierarchyItem
			unmarshalArg(req.Args[0], &p0)
			orchestrator := s.getDenoCrossProjectOrchestrator(ctx, pe, p0.Uri)
			return languageService.ProvideCallHierarchyIncomingCalls(ctx, &p0, orchestrator)
		case "ProvideCallHierarchyOutgoingCalls":
			var p0 lsproto.CallHierarchyItem
			unmarshalArg(req.Args[0], &p0)
			return languageService.ProvideCallHierarchyOutgoingCalls(ctx, &p0)
		case "ProvidePrepareCallHierarchy":
			var p0 lsproto.DocumentUri
			unmarshalArg(req.Args[0], &p0)
			var p1 lsproto.Position
			unmarshalArg(req.Args[1], &p1)
			return languageService.ProvidePrepareCallHierarchy(ctx, p0, p1)
		case "ProvideRename":
			var p0 lsproto.RenameParams
			unmarshalArg(req.Args[0], &p0)
			orchestrator := s.getDenoCrossProjectOrchestrator(ctx, pe, p0.TextDocument.Uri)
			return languageService.ProvideRename(ctx, &p0, orchestrator)
		case "ProvideSelectionRanges":
			var p0 lsproto.SelectionRangeParams
			unmarshalArg(req.Args[0], &p0)
			return languageService.ProvideSelectionRanges(ctx, &p0)
		case "ProvideSignatureHelp":
			var p0 lsproto.DocumentUri
			unmarshalArg(req.Args[0], &p0)
			var p1 lsproto.Position
			unmarshalArg(req.Args[1], &p1)
			var p2 lsproto.SignatureHelpContext
			unmarshalArg(req.Args[2], &p2)
			return languageService.ProvideSignatureHelp(ctx, p0, p1, &p2)
		case "ProvideInlayHint":
			var p0 lsproto.InlayHintParams
			unmarshalArg(req.Args[0], &p0)
			return languageService.ProvideInlayHint(ctx, &p0)
		default:
			return nil, fmt.Errorf("Unsupported method: %s", req.Name)
		}
	} else if params.GetAmbientModules != nil {
		req := params.GetAmbientModules
		pe, _, err := s.getDenoLanguageService(req.CompilerOptionsKey, req.NotebookUri)
		if err != nil {
			return nil, err
		}
		checker, done := pe.program.GetTypeChecker(ctx)
		defer done()
		symbols := checker.GetAmbientModules(nil)
		return core.Map(symbols, func(s *ast.Symbol) string { return s.Name }), nil
	} else if params.WorkspaceSymbol != nil {
		req := params.WorkspaceSymbol
		var programs []*compiler.Program
		var firstHost *DenoLanguageServiceHost
		for _, pe := range s.deno.programEntries.byCompilerOptionsKey {
			programs = append(programs, pe.program)
			if firstHost == nil {
				firstHost = NewDenoLanguageServiceHost(pe.vfs, pe.userPreferences, pe.autoImportRegistry)
			}
		}
		for _, pe := range s.deno.programEntries.byNotebookUri {
			programs = append(programs, pe.program)
			if firstHost == nil {
				firstHost = NewDenoLanguageServiceHost(pe.vfs, pe.userPreferences, pe.autoImportRegistry)
			}
		}
		if firstHost == nil {
			return nil, fmt.Errorf("No program entries available")
		}
		return ls.ProvideWorkspaceSymbols(ctx, programs, firstHost.Converters(), firstHost.GetPreferences(""), req.Query)
	}
	return nil, nil
}

func (s *Server) getDenoLanguageService(compilerOptionsKey string, notebookUri *string) (*DenoProgramEntry, *ls.LanguageService, error) {
	var pe *DenoProgramEntry
	if notebookUri != nil {
		pe = s.deno.programEntries.byNotebookUri[*notebookUri]
	}
	if pe == nil {
		pe = s.deno.programEntries.byCompilerOptionsKey[compilerOptionsKey]
	}
	if pe == nil {
		if notebookUri != nil {
			return nil, nil, fmt.Errorf("Couldn't find program entry for key: %s and notebook %s", compilerOptionsKey, *notebookUri)
		}
		return nil, nil, fmt.Errorf("Couldn't find program entry for key: %s", compilerOptionsKey)
	}

	host := NewDenoLanguageServiceHost(pe.vfs, pe.userPreferences, pe.autoImportRegistry)
	languageService := ls.NewLanguageService(pe.projectPath, pe.program, host, "")
	return pe, languageService, nil
}

func (s *Server) getDenoCrossProjectOrchestrator(ctx context.Context, pe *DenoProgramEntry, initialUri lsproto.DocumentUri) *DenoCrossProjectOrchestrator {
	host := NewDenoLanguageServiceHost(pe.vfs, pe.userPreferences, pe.autoImportRegistry)
	return &DenoCrossProjectOrchestrator{
		server:              s,
		ctx:                 ctx,
		defaultProgramEntry: pe,
		defaultHost:         host,
		initialUri:          initialUri,
	}
}

func (s *Server) createDenoProgramEntry(ctx context.Context, compilerOptionsKey string, notebookUri *string, projectConfig *lsproto.DenoProjectConfig) (*DenoProgramEntry, error) {
	denoVFS := NewDenoVFS(s, ctx, compilerOptionsKey, notebookUri)
	wrappedFS := bundled.WrapFS(denoVFS)
	baseHost := compiler.NewCompilerHost(
		s.cwd,
		wrappedFS,
		bundled.LibPath(),
		nil, // extendedConfigCache
		nil, // trace
	)
	// Wrap the host to intercept resolution
	compilerHost := &denoCompilerHost{
		inner:              baseHost,
		server:             s,
		vfs:                denoVFS,
		compilerOptionsKey: compilerOptionsKey,
		notebookUri:        notebookUri,
	}
	fileNames := make([]string, len(projectConfig.Files))
	for i, file := range projectConfig.Files {
		fileNames[i] = file.FileName()
	}
	parsedCommandLine := &tsoptions.ParsedCommandLine{
		ParsedConfig: &core.ParsedOptions{
			CompilerOptions: projectConfig.CompilerOptions,
			FileNames:       fileNames,
		},
	}
	program := compiler.NewProgram(compiler.ProgramOptions{
		Host:   compilerHost,
		Config: parsedCommandLine,
		CreateCheckerPool: func(p *compiler.Program) compiler.CheckerPool {
			return project.DenoNewCheckerPool(4, p, func(msg string) {})
		},
	})

	// This is not used as an actual path, just a unique id.
	var projectPath tspath.Path
	if notebookUri != nil {
		projectPath = tspath.Path(*notebookUri)
	} else {
		projectPath = tspath.Path(compilerOptionsKey)
	}

	userPreferences := lsutil.NewDefaultUserPreferences()
	if config, ok := (*projectConfig.UserPreferences).(map[string]any); ok {
		userPreferences.ParseWorker(config)
	}

	toPath := func(fileName string) tspath.Path {
		return tspath.ToPath(fileName, s.cwd, denoVFS.UseCaseSensitiveFileNames())
	}
	autoImportRegistry := autoimport.NewRegistry(toPath, userPreferences)

	// Initialize the registry with a project bucket by calling Clone with the open files
	autoImportHost := &denoAutoImportHost{
		vfs:         wrappedFS,
		cwd:         s.cwd,
		projectPath: projectPath,
		program:     program,
	}
	openFiles := make(map[tspath.Path]string)
	var firstFilePath tspath.Path
	for _, file := range projectConfig.Files {
		fileName := file.FileName()
		filePath := toPath(fileName)
		openFiles[filePath] = fileName
		if firstFilePath == "" {
			firstFilePath = filePath
		}
	}
	autoImportRegistry, _ = autoImportRegistry.Clone(ctx, autoimport.RegistryChange{
		OpenFiles:     openFiles,
		RequestedFile: firstFilePath, // Triggers updateIndexes to build the project bucket
	}, autoImportHost, nil)

	return &DenoProgramEntry{
		program:            program,
		projectPath:        projectPath,
		compilerOptionsKey: compilerOptionsKey,
		notebookUri:        notebookUri,
		compilerOptions:    projectConfig.CompilerOptions,
		userPreferences:    userPreferences,
		vfs:                denoVFS,
		autoImportRegistry: autoImportRegistry,
	}, nil
}

func (s *Server) handleInitialize(ctx context.Context, params *lsproto.InitializeParams, _ *lsproto.RequestMessage) (lsproto.InitializeResponse, error) {
	if s.initializeParams != nil {
		return nil, lsproto.ErrorCodeInvalidRequest
	}

	s.initStarted.Store(true)

	s.initializeParams = params
	s.clientCapabilities = lsproto.ResolveClientCapabilities(params.Capabilities)

	capabilitiesJSON, err := json.MarshalIndent(&s.clientCapabilities, "", "\t")
	if err != nil {
		return nil, err
	}
	s.logger.Info("Resolved client capabilities: " + string(capabilitiesJSON))

	s.positionEncoding = lsproto.PositionEncodingKindUTF16
	if slices.Contains(s.clientCapabilities.General.PositionEncodings, lsproto.PositionEncodingKindUTF8) {
		s.positionEncoding = lsproto.PositionEncodingKindUTF8
	}

	if s.initializeParams.Locale != nil {
		s.locale, _ = locale.Parse(*s.initializeParams.Locale)
	}

	if s.initializeParams.Trace != nil && *s.initializeParams.Trace == "verbose" {
		s.logger.SetVerbose(true)
	}

	response := &lsproto.InitializeResult{
		ServerInfo: &lsproto.ServerInfo{
			Name:    "typescript-go",
			Version: new(core.Version()),
		},
		Capabilities: &lsproto.ServerCapabilities{
			PositionEncoding: new(s.positionEncoding),
			TextDocumentSync: &lsproto.TextDocumentSyncOptionsOrKind{
				Options: &lsproto.TextDocumentSyncOptions{
					OpenClose: new(true),
					Change:    new(lsproto.TextDocumentSyncKindIncremental),
					Save: &lsproto.BooleanOrSaveOptions{
						Boolean: new(true),
					},
				},
			},
			HoverProvider: &lsproto.BooleanOrHoverOptions{
				Boolean: new(true),
			},
			DefinitionProvider: &lsproto.BooleanOrDefinitionOptions{
				Boolean: new(true),
			},
			TypeDefinitionProvider: &lsproto.BooleanOrTypeDefinitionOptionsOrTypeDefinitionRegistrationOptions{
				Boolean: new(true),
			},
			ReferencesProvider: &lsproto.BooleanOrReferenceOptions{
				Boolean: new(true),
			},
			ImplementationProvider: &lsproto.BooleanOrImplementationOptionsOrImplementationRegistrationOptions{
				Boolean: new(true),
			},
			DiagnosticProvider: &lsproto.DiagnosticOptionsOrRegistrationOptions{
				Options: &lsproto.DiagnosticOptions{
					InterFileDependencies: true,
				},
			},
			CompletionProvider: &lsproto.CompletionOptions{
				TriggerCharacters: &ls.TriggerCharacters,
				ResolveProvider:   new(true),
				// !!! other options
			},
			SignatureHelpProvider: &lsproto.SignatureHelpOptions{
				TriggerCharacters: &[]string{"(", ","},
			},
			DocumentFormattingProvider: &lsproto.BooleanOrDocumentFormattingOptions{
				Boolean: new(true),
			},
			DocumentRangeFormattingProvider: &lsproto.BooleanOrDocumentRangeFormattingOptions{
				Boolean: new(true),
			},
			DocumentOnTypeFormattingProvider: &lsproto.DocumentOnTypeFormattingOptions{
				FirstTriggerCharacter: "{",
				MoreTriggerCharacter:  &[]string{"}", ";", "\n"},
			},
			WorkspaceSymbolProvider: &lsproto.BooleanOrWorkspaceSymbolOptions{
				Boolean: new(true),
			},
			DocumentSymbolProvider: &lsproto.BooleanOrDocumentSymbolOptions{
				Boolean: new(true),
			},
			FoldingRangeProvider: &lsproto.BooleanOrFoldingRangeOptionsOrFoldingRangeRegistrationOptions{
				Boolean: new(true),
			},
			RenameProvider: &lsproto.BooleanOrRenameOptions{
				Boolean: new(true),
			},
			DocumentHighlightProvider: &lsproto.BooleanOrDocumentHighlightOptions{
				Boolean: new(true),
			},
			SelectionRangeProvider: &lsproto.BooleanOrSelectionRangeOptionsOrSelectionRangeRegistrationOptions{
				Boolean: new(true),
			},
			InlayHintProvider: &lsproto.BooleanOrInlayHintOptionsOrInlayHintRegistrationOptions{
				Boolean: new(true),
			},
			CodeLensProvider: &lsproto.CodeLensOptions{
				ResolveProvider: new(true),
			},
			CodeActionProvider: &lsproto.BooleanOrCodeActionOptions{
				CodeActionOptions: &lsproto.CodeActionOptions{
					CodeActionKinds: &[]lsproto.CodeActionKind{
						lsproto.CodeActionKindQuickFix,
						lsproto.CodeActionKindSourceOrganizeImports,
						lsproto.CodeActionKindSourceRemoveUnusedImports,
						lsproto.CodeActionKindSourceSortImports,
					},
				},
			},
			CallHierarchyProvider: &lsproto.BooleanOrCallHierarchyOptionsOrCallHierarchyRegistrationOptions{
				Boolean: new(true),
			},
		},
	}

	return response, nil
}

func (s *Server) handleInitialized(ctx context.Context, params *lsproto.InitializedParams) error {
	if s.clientCapabilities.Workspace.DidChangeWatchedFiles.DynamicRegistration {
		s.watchEnabled = true
	}

	cwd := s.cwd
	if s.clientCapabilities.Workspace.WorkspaceFolders &&
		s.initializeParams.WorkspaceFolders != nil &&
		s.initializeParams.WorkspaceFolders.WorkspaceFolders != nil &&
		len(*s.initializeParams.WorkspaceFolders.WorkspaceFolders) == 1 {
		cwd = lsproto.DocumentUri((*s.initializeParams.WorkspaceFolders.WorkspaceFolders)[0].Uri).FileName()
	} else if s.initializeParams.RootUri.DocumentUri != nil {
		cwd = s.initializeParams.RootUri.DocumentUri.FileName()
	} else if s.initializeParams.RootPath != nil && s.initializeParams.RootPath.String != nil {
		cwd = *s.initializeParams.RootPath.String
	}
	if !tspath.PathIsAbsolute(cwd) {
		cwd = s.cwd
	}

	var disablePushDiagnostics bool
	if s.initializeParams != nil && s.initializeParams.InitializationOptions != nil {
		if s.initializeParams.InitializationOptions.DisablePushDiagnostics != nil {
			disablePushDiagnostics = *s.initializeParams.InitializationOptions.DisablePushDiagnostics
		}
	}

	s.session = project.NewSession(&project.SessionInit{
		BackgroundCtx: s.backgroundCtx,
		Options: &project.SessionOptions{
			CurrentDirectory:       cwd,
			DefaultLibraryPath:     s.defaultLibraryPath,
			TypingsLocation:        s.typingsLocation,
			PositionEncoding:       s.positionEncoding,
			WatchEnabled:           s.watchEnabled,
			LoggingEnabled:         true,
			DebounceDelay:          500 * time.Millisecond,
			PushDiagnosticsEnabled: !disablePushDiagnostics,
			Locale:                 s.locale,
			MakeHost:               project.NewProjectHost,
		},
		FS:          s.fs,
		Logger:      s.logger,
		Client:      s,
		NpmExecutor: s,
		ParseCache:  s.parseCache,
	})

	userPreferences, err := s.RequestConfiguration(ctx)
	if err != nil {
		return err
	}
	s.session.InitializeWithUserConfig(userPreferences)

	_, err = sendClientRequest(ctx, s, lsproto.ClientRegisterCapabilityInfo, &lsproto.RegistrationParams{
		Registrations: []*lsproto.Registration{
			{
				Id:     "typescript-config-watch-id",
				Method: string(lsproto.MethodWorkspaceDidChangeConfiguration),
				RegisterOptions: &lsproto.RegisterOptions{
					DidChangeConfiguration: &lsproto.DidChangeConfigurationRegistrationOptions{
						Section: &lsproto.StringOrStrings{
							Strings: &[]string{"js/ts", "typescript", "javascript", "editor"},
						},
					},
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to register configuration change watcher: %w", err)
	}

	// !!! temporary.
	// Remove when we have `handleDidChangeConfiguration`/implicit project config support
	// derived from 'js/ts.implicitProjectConfig.*'.
	if s.compilerOptionsForInferredProjects != nil {
		s.session.DidChangeCompilerOptionsForInferredProjects(ctx, s.compilerOptionsForInferredProjects)
	}

	close(s.initComplete)
	return nil
}

func (s *Server) handleShutdown(ctx context.Context, params any, _ *lsproto.RequestMessage) (lsproto.ShutdownResponse, error) {
	s.session.Close()
	return lsproto.ShutdownResponse{}, nil
}

func (s *Server) handleExit(ctx context.Context, params any) error {
	return io.EOF
}

func (s *Server) handleDidChangeWorkspaceConfiguration(ctx context.Context, params *lsproto.DidChangeConfigurationParams) error {
	if params.Settings == nil {
		return nil
	} else if settings, ok := params.Settings.(map[string]any); ok {
		s.session.Configure(lsutil.ParseNewUserConfig(settings))
	}
	return nil
}

func (s *Server) handleDidOpen(ctx context.Context, params *lsproto.DidOpenTextDocumentParams) error {
	s.session.DidOpenFile(ctx, params.TextDocument.Uri, params.TextDocument.Version, params.TextDocument.Text, params.TextDocument.LanguageId)
	return nil
}

func (s *Server) handleDidChange(ctx context.Context, params *lsproto.DidChangeTextDocumentParams) error {
	s.session.DidChangeFile(ctx, params.TextDocument.Uri, params.TextDocument.Version, params.ContentChanges)
	return nil
}

func (s *Server) handleDidSave(ctx context.Context, params *lsproto.DidSaveTextDocumentParams) error {
	s.session.DidSaveFile(ctx, params.TextDocument.Uri)
	return nil
}

func (s *Server) handleDidClose(ctx context.Context, params *lsproto.DidCloseTextDocumentParams) error {
	s.session.DidCloseFile(ctx, params.TextDocument.Uri)
	return nil
}

func (s *Server) handleDidChangeWatchedFiles(ctx context.Context, params *lsproto.DidChangeWatchedFilesParams) error {
	s.session.DidChangeWatchedFiles(ctx, params.Changes)
	return nil
}

func (s *Server) handleSetTrace(ctx context.Context, params *lsproto.SetTraceParams) error {
	switch params.Value {
	case "verbose":
		s.logger.SetVerbose(true)
	case "messages":
		s.logger.SetVerbose(false)
	case "off":
		// !!! logging cannot be completely turned off for now
		s.logger.SetVerbose(false)
	default:
		return fmt.Errorf("unknown trace value: %s", params.Value)
	}
	return nil
}

func (s *Server) handleDocumentDiagnostic(ctx context.Context, ls *ls.LanguageService, params *lsproto.DocumentDiagnosticParams) (lsproto.DocumentDiagnosticResponse, error) {
	return ls.ProvideDiagnostics(ctx, params.TextDocument.Uri)
}

func (s *Server) handleHover(ctx context.Context, ls *ls.LanguageService, params *lsproto.HoverParams) (lsproto.HoverResponse, error) {
	return ls.ProvideHover(ctx, params.TextDocument.Uri, params.Position)
}

func (s *Server) handleSignatureHelp(ctx context.Context, languageService *ls.LanguageService, params *lsproto.SignatureHelpParams) (lsproto.SignatureHelpResponse, error) {
	return languageService.ProvideSignatureHelp(
		ctx,
		params.TextDocument.Uri,
		params.Position,
		params.Context,
	)
}

func (s *Server) handleFoldingRange(ctx context.Context, ls *ls.LanguageService, params *lsproto.FoldingRangeParams) (lsproto.FoldingRangeResponse, error) {
	return ls.ProvideFoldingRange(ctx, params.TextDocument.Uri)
}

func (s *Server) handleClosingTagCompletion(ctx context.Context, ls *ls.LanguageService, params *lsproto.TextDocumentPositionParams) (lsproto.CustomClosingTagCompletionResponse, error) {
	return ls.ProvideClosingTagCompletion(ctx, params)
}

func (s *Server) handleDefinition(ctx context.Context, ls *ls.LanguageService, params *lsproto.DefinitionParams) (lsproto.DefinitionResponse, error) {
	return ls.ProvideDefinition(ctx, params.TextDocument.Uri, params.Position)
}

func (s *Server) handleTypeDefinition(ctx context.Context, ls *ls.LanguageService, params *lsproto.TypeDefinitionParams) (lsproto.TypeDefinitionResponse, error) {
	return ls.ProvideTypeDefinition(ctx, params.TextDocument.Uri, params.Position)
}

func (s *Server) handleCompletion(ctx context.Context, languageService *ls.LanguageService, params *lsproto.CompletionParams) (lsproto.CompletionResponse, error) {
	return languageService.ProvideCompletion(
		ctx,
		params.TextDocument.Uri,
		params.Position,
		params.Context,
	)
}

func (s *Server) handleCompletionItemResolve(ctx context.Context, params *lsproto.CompletionItem, reqMsg *lsproto.RequestMessage) (lsproto.CompletionResolveResponse, error) {
	data := params.Data
	languageService, err := s.session.GetLanguageService(ctx, lsconv.FileNameToDocumentURI(data.FileName))
	if err != nil {
		return nil, err
	}
	defer s.recover(reqMsg)
	return languageService.ResolveCompletionItem(
		ctx,
		params,
		data,
	)
}

func (s *Server) handleDocumentFormat(ctx context.Context, ls *ls.LanguageService, params *lsproto.DocumentFormattingParams) (lsproto.DocumentFormattingResponse, error) {
	return ls.ProvideFormatDocument(
		ctx,
		params.TextDocument.Uri,
		params.Options,
	)
}

func (s *Server) handleDocumentRangeFormat(ctx context.Context, ls *ls.LanguageService, params *lsproto.DocumentRangeFormattingParams) (lsproto.DocumentRangeFormattingResponse, error) {
	return ls.ProvideFormatDocumentRange(
		ctx,
		params.TextDocument.Uri,
		params.Options,
		params.Range,
	)
}

func (s *Server) handleDocumentOnTypeFormat(ctx context.Context, ls *ls.LanguageService, params *lsproto.DocumentOnTypeFormattingParams) (lsproto.DocumentOnTypeFormattingResponse, error) {
	return ls.ProvideFormatDocumentOnType(
		ctx,
		params.TextDocument.Uri,
		params.Options,
		params.Position,
		params.Ch,
	)
}

func (s *Server) handleWorkspaceSymbol(ctx context.Context, params *lsproto.WorkspaceSymbolParams, reqMsg *lsproto.RequestMessage) (lsproto.WorkspaceSymbolResponse, error) {
	snapshot := s.session.GetSnapshotLoadingProjectTree(ctx, nil)
	defer s.recover(reqMsg)

	programs := core.Map(snapshot.ProjectCollection.Projects(), (*project.Project).GetProgram)
	return ls.ProvideWorkspaceSymbols(
		ctx,
		programs,
		snapshot.Converters(),
		snapshot.UserPreferences(),
		params.Query)
}

func (s *Server) handleDocumentSymbol(ctx context.Context, ls *ls.LanguageService, params *lsproto.DocumentSymbolParams) (lsproto.DocumentSymbolResponse, error) {
	return ls.ProvideDocumentSymbols(ctx, params.TextDocument.Uri)
}

func (s *Server) handleDocumentHighlight(ctx context.Context, ls *ls.LanguageService, params *lsproto.DocumentHighlightParams) (lsproto.DocumentHighlightResponse, error) {
	return ls.ProvideDocumentHighlights(ctx, params.TextDocument.Uri, params.Position)
}

func (s *Server) handleSelectionRange(ctx context.Context, ls *ls.LanguageService, params *lsproto.SelectionRangeParams) (lsproto.SelectionRangeResponse, error) {
	return ls.ProvideSelectionRanges(ctx, params)
}

func (s *Server) handleCodeAction(ctx context.Context, ls *ls.LanguageService, params *lsproto.CodeActionParams) (lsproto.CodeActionResponse, error) {
	return ls.ProvideCodeActions(ctx, params)
}

func (s *Server) handleInlayHint(
	ctx context.Context,
	languageService *ls.LanguageService,
	params *lsproto.InlayHintParams,
) (lsproto.InlayHintResponse, error) {
	return languageService.ProvideInlayHint(ctx, params)
}

func (s *Server) handleCodeLens(ctx context.Context, ls *ls.LanguageService, params *lsproto.CodeLensParams) (lsproto.CodeLensResponse, error) {
	return ls.ProvideCodeLenses(ctx, params.TextDocument.Uri)
}

func (s *Server) handleCodeLensResolve(ctx context.Context, codeLens *lsproto.CodeLens, reqMsg *lsproto.RequestMessage) (*lsproto.CodeLens, error) {
	defaultLs, orchestrator, err := s.getLanguageServiceAndCrossProjectOrchestrator(ctx, codeLens.Data.Uri, reqMsg)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		// This can happen if a codeLens/resolve request comes in after a program change.
		// While it's true that handlers should latch onto a specific snapshot
		// while processing requests, we just set `Data.Uri` based on
		// some older snapshot's contents. The content could have been modified,
		// or the file itself could have been removed from the session entirely.
		// Note this won't bail out on every change, but will prevent crashing
		// based on non-existent files and line maps from shortened files.
		return codeLens, lsproto.ErrorCodeContentModified
	}
	defer s.recover(reqMsg)
	return defaultLs.ResolveCodeLens(
		ctx,
		codeLens,
		s.initializeParams.InitializationOptions.CodeLensShowLocationsCommandName,
		orchestrator,
	)
}

func (s *Server) handlePrepareCallHierarchy(
	ctx context.Context,
	languageService *ls.LanguageService,
	params *lsproto.CallHierarchyPrepareParams,
) (lsproto.CallHierarchyPrepareResponse, error) {
	return languageService.ProvidePrepareCallHierarchy(ctx, params.TextDocument.Uri, params.Position)
}

func (s *Server) handleCallHierarchyIncomingCalls(
	ctx context.Context,
	params *lsproto.CallHierarchyIncomingCallsParams,
	reqMsg *lsproto.RequestMessage,
) (lsproto.CallHierarchyIncomingCallsResponse, error) {
	defaultLs, orchestrator, err := s.getLanguageServiceAndCrossProjectOrchestrator(ctx, params.Item.Uri, reqMsg)
	if err != nil {
		return lsproto.CallHierarchyIncomingCallsOrNull{}, err
	}
	return defaultLs.ProvideCallHierarchyIncomingCalls(ctx, params.Item, orchestrator)
}

func (s *Server) handleCallHierarchyOutgoingCalls(
	ctx context.Context,
	params *lsproto.CallHierarchyOutgoingCallsParams,
	_ *lsproto.RequestMessage,
) (lsproto.CallHierarchyOutgoingCallsResponse, error) {
	languageService, err := s.session.GetLanguageService(ctx, params.Item.Uri)
	if err != nil {
		return lsproto.CallHierarchyOutgoingCallsOrNull{}, err
	}
	return languageService.ProvideCallHierarchyOutgoingCalls(ctx, params.Item)
}

func (s *Server) handleInitializeAPISession(ctx context.Context, params *lsproto.InitializeAPISessionParams, _ *lsproto.RequestMessage) (lsproto.CustomInitializeAPISessionResponse, error) {
	s.apiSessionsMu.Lock()
	defer s.apiSessionsMu.Unlock()

	if s.apiSessions == nil {
		s.apiSessions = make(map[string]*api.Session)
	}

	var apiSession *api.Session
	apiSession = api.NewSession(s.session, nil)

	// Use provided pipe path or generate a unique one
	var pipePath string
	if params.Pipe != nil && *params.Pipe != "" {
		pipePath = *params.Pipe
	} else {
		pipePath = s.generateAPIPipePath()
	}

	transport, err := api.NewPipeTransport(pipePath)
	if err != nil {
		return nil, fmt.Errorf("failed to create API transport: %w", err)
	}

	// Start accepting connections in the background
	go func() {
		defer func() {
			apiSession.Close()
			s.removeAPISession(apiSession.ID())
		}()

		rwc, acceptErr := transport.Accept()
		_ = transport.Close()
		if acceptErr != nil {
			s.logger.Errorf("API session %s: failed to accept connection: %v", apiSession.ID(), acceptErr)
			return
		}

		// Create a cancellable context for the API connection
		apiCtx, apiCancel := context.WithCancel(s.backgroundCtx)
		defer apiCancel()

		// Run the connection with panic recovery
		defer func() {
			if r := recover(); r != nil {
				stack := debug.Stack()
				s.logger.Errorf("API session %s: panic: %v\n%s", apiSession.ID(), r, string(stack))
				// Cancel the context to shut down the connection
				apiCancel()
				// Close the underlying connection
				rwc.Close()
			}
		}()

		conn := api.NewAsyncConn(rwc, apiSession)
		if apiErr := conn.Run(apiCtx); apiErr != nil {
			s.logger.Errorf("API session %s: %v", apiSession.ID(), apiErr)
		}
	}()

	s.apiSessions[apiSession.ID()] = apiSession

	return &lsproto.InitializeAPISessionResult{
		SessionId: apiSession.ID(),
		Pipe:      pipePath,
	}, nil
}

func (s *Server) generateAPIPipePath() string {
	// Generate a high-entropy path using time and random source
	now := time.Now().UnixNano()
	rnd := rand.Uint64()
	return api.GeneratePipePath(fmt.Sprintf("tsgo-api-%x-%x", now, rnd))
}

func (s *Server) removeAPISession(id string) {
	s.apiSessionsMu.Lock()
	defer s.apiSessionsMu.Unlock()
	delete(s.apiSessions, id)
}

// !!! temporary; remove when we have `handleDidChangeConfiguration`/implicit project config support
func (s *Server) SetCompilerOptionsForInferredProjects(ctx context.Context, options *core.CompilerOptions) {
	s.compilerOptionsForInferredProjects = options
	if s.session != nil {
		s.session.DidChangeCompilerOptionsForInferredProjects(ctx, options)
	}
}

// NpmInstall implements ata.NpmExecutor
func (s *Server) NpmInstall(cwd string, args []string) ([]byte, error) {
	return s.npmInstall(cwd, args)
}

// Developer/debugging command handlers

func (s *Server) handleRunGC(_ context.Context, _ any, _ *lsproto.RequestMessage) (lsproto.RunGCResponse, error) {
	pprof.RunGC()
	s.logger.Info("GC triggered")
	return lsproto.Null{}, nil
}

func (s *Server) handleSaveHeapProfile(_ context.Context, params *lsproto.ProfileParams, _ *lsproto.RequestMessage) (*lsproto.ProfileResult, error) {
	filePath, err := pprof.SaveHeapProfile(params.Dir)
	if err != nil {
		return nil, err
	}
	s.logger.Info("Heap profile saved to: ", filePath)
	return &lsproto.ProfileResult{File: filePath}, nil
}

func (s *Server) handleSaveAllocProfile(_ context.Context, params *lsproto.ProfileParams, _ *lsproto.RequestMessage) (*lsproto.ProfileResult, error) {
	filePath, err := pprof.SaveAllocProfile(params.Dir)
	if err != nil {
		return nil, err
	}
	s.logger.Info("Allocation profile saved to: ", filePath)
	return &lsproto.ProfileResult{File: filePath}, nil
}

func (s *Server) handleStartCPUProfile(_ context.Context, params *lsproto.ProfileParams, _ *lsproto.RequestMessage) (lsproto.StartCPUProfileResponse, error) {
	err := s.cpuProfiler.StartCPUProfile(params.Dir)
	if err != nil {
		return lsproto.Null{}, err
	}
	s.logger.Info("CPU profiling started, will save to: ", params.Dir)
	return lsproto.Null{}, nil
}

func (s *Server) handleStopCPUProfile(_ context.Context, _ any, _ *lsproto.RequestMessage) (*lsproto.ProfileResult, error) {
	filePath, err := s.cpuProfiler.StopCPUProfile()
	if err != nil {
		return nil, err
	}
	s.logger.Info("CPU profile saved to: ", filePath)
	return &lsproto.ProfileResult{File: filePath}, nil
}
