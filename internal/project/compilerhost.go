package project

import (
	"time"

	"github.com/microsoft/typescript-go/internal/ast"
	"github.com/microsoft/typescript-go/internal/collections"
	"github.com/microsoft/typescript-go/internal/compiler"
	"github.com/microsoft/typescript-go/internal/core"
	"github.com/microsoft/typescript-go/internal/diagnostics"
	"github.com/microsoft/typescript-go/internal/module"
	"github.com/microsoft/typescript-go/internal/project/logging"
	"github.com/microsoft/typescript-go/internal/tsoptions"
	"github.com/microsoft/typescript-go/internal/tspath"
	"github.com/microsoft/typescript-go/internal/vfs"
)

type ProjectHost interface {
	compiler.CompilerHost
	Builder() *ProjectCollectionBuilder
	SessionOptions() *SessionOptions
	SeenFiles() *collections.SyncSet[tspath.Path]
	UpdateSeenFiles(*collections.SyncSet[tspath.Path])
	Freeze(snapshotFS *SnapshotFS, configFileRegistry *ConfigFileRegistry)
	CompilerFS() *CompilerFS
	SourceFS() *SourceFS
}

var (
	_ compiler.CompilerHost = (*compilerHost)(nil)
	_ ProjectHost           = (*compilerHost)(nil)
)

type compilerHost struct {
	configFilePath   tspath.Path
	currentDirectory string
	sessionOptions   *SessionOptions

	sourceFS           *SourceFS
	compilerFS         *CompilerFS
	configFileRegistry *ConfigFileRegistry

	project *Project
	builder *ProjectCollectionBuilder
	logger  *logging.LogTree
}

// TypesNodeIgnorableNames implements compiler.CompilerHost.
func (c *compilerHost) GetDenoForkContextInfo() ast.DenoForkContextInfo {
	return ast.DenoForkContextInfo{}
}

// IsNodeSourceFile implements compiler.CompilerHost.
func (c *compilerHost) IsNodeSourceFile(path tspath.Path) bool {
	return false
}

type builderFileSource struct {
	seenFiles         *collections.SyncSet[tspath.Path]
	snapshotFSBuilder *snapshotFSBuilder
}

func (c *builderFileSource) GetFile(fileName string) FileHandle {
	path := c.snapshotFSBuilder.toPath(fileName)
	c.seenFiles.Add(path)
	return c.snapshotFSBuilder.GetFileByPath(fileName, path)
}

func (c *builderFileSource) GetFileByPath(fileName string, path tspath.Path) FileHandle {
	c.seenFiles.Add(path)
	return c.snapshotFSBuilder.GetFileByPath(fileName, path)
}

func (c *builderFileSource) FS() vfs.FS {
	return c.snapshotFSBuilder.FS()
}

func (c *builderFileSource) GetAccessibleEntries(path string) vfs.Entries {
	return c.snapshotFSBuilder.GetAccessibleEntries(path)
}

func NewProjectHost(
	currentDirectory string,
	project *Project,
	builder *ProjectCollectionBuilder,
	logger *logging.LogTree,
) ProjectHost {
	seenFiles := &collections.SyncSet[tspath.Path]{}
	compilerFS := &CompilerFS{
		source: &builderFileSource{
			seenFiles:         seenFiles,
			snapshotFSBuilder: builder.fs,
		},
	}

	return &compilerHost{
		configFilePath:   project.configFilePath,
		currentDirectory: currentDirectory,
		sessionOptions:   builder.sessionOptions,

		sourceFS:   newSourceFS(true, builder.fs, builder.toPath),
		compilerFS: compilerFS,

		project: project,
		builder: builder,
		logger:  logger,
	}
}

// freeze clears references to mutable state to make the compilerHost safe for use
// after the snapshot has been finalized. See the usage in snapshot.go for more details.
func (c *compilerHost) freeze(snapshotFS *SnapshotFS, configFileRegistry *ConfigFileRegistry) {
	if c.builder == nil {
		panic("freeze can only be called once")
	}
	c.sourceFS.source = snapshotFS
	c.sourceFS.DisableTracking()
	c.configFileRegistry = configFileRegistry
	c.builder = nil
	c.project = nil
	c.logger = nil
}

func (c *compilerHost) ensureAlive() {
	if c.builder == nil || c.project == nil {
		panic("method must not be called after snapshot initialization")
	}
}

// DefaultLibraryPath implements compiler.CompilerHost.
func (c *compilerHost) DefaultLibraryPath() string {
	return c.sessionOptions.DefaultLibraryPath
}

// FS implements compiler.CompilerHost.
func (c *compilerHost) FS() vfs.FS {
	return c.sourceFS
}

// GetCurrentDirectory implements compiler.CompilerHost.
func (c *compilerHost) GetCurrentDirectory() string {
	return c.currentDirectory
}

// GetResolvedProjectReference implements compiler.CompilerHost.
func (c *compilerHost) GetResolvedProjectReference(fileName string, path tspath.Path) *tsoptions.ParsedCommandLine {
	if c.builder == nil {
		return c.configFileRegistry.GetConfig(path)
	} else {
		// acquireConfigForProject will bypass sourceFS, so track the file here.
		c.sourceFS.Track(fileName)
		return c.builder.configFileRegistryBuilder.acquireConfigForProject(fileName, path, c.project, c.logger)
	}
}

// GetSourceFile implements compiler.CompilerHost. Files are cached in parseCache;
// ref counting is handled at the snapshot level after program construction.
func (c *compilerHost) GetSourceFile(opts ast.SourceFileParseOptions) *ast.SourceFile {
	c.ensureAlive()
	if fh := c.sourceFS.GetFileByPath(opts.FileName, opts.Path); fh != nil {
		return c.builder.parseCache.Load(NewParseCacheKey(opts, fh.Hash(), fh.Kind()), fh)
	}
	return nil
}

// Trace implements compiler.CompilerHost.
func (c *compilerHost) Trace(msg *diagnostics.Message, args ...any) {
	panic("unimplemented")
}

var _ vfs.FS = (*CompilerFS)(nil)

type CompilerFS struct {
	source FileSource
}

// DirectoryExists implements vfs.FS.
func (fs *CompilerFS) DirectoryExists(path string) bool {
	return fs.source.FS().DirectoryExists(path)
}

// FileExists implements vfs.FS.
func (fs *CompilerFS) FileExists(path string) bool {
	if fh := fs.source.GetFile(path); fh != nil {
		return true
	}
	return fs.source.FS().FileExists(path)
}

// GetAccessibleEntries implements vfs.FS.
func (fs *CompilerFS) GetAccessibleEntries(path string) vfs.Entries {
	return fs.source.FS().GetAccessibleEntries(path)
}

// ReadFile implements vfs.FS.
func (fs *CompilerFS) ReadFile(path string) (contents string, ok bool) {
	if fh := fs.source.GetFile(path); fh != nil {
		return fh.Content(), true
	}
	return "", false
}

// Realpath implements vfs.FS.
func (fs *CompilerFS) Realpath(path string) string {
	return fs.source.FS().Realpath(path)
}

// Stat implements vfs.FS.
func (fs *CompilerFS) Stat(path string) vfs.FileInfo {
	return fs.source.FS().Stat(path)
}

// UseCaseSensitiveFileNames implements vfs.FS.
func (fs *CompilerFS) UseCaseSensitiveFileNames() bool {
	return fs.source.FS().UseCaseSensitiveFileNames()
}

// WalkDir implements vfs.FS.
func (fs *CompilerFS) WalkDir(root string, walkFn vfs.WalkDirFunc) error {
	panic("unimplemented")
}

// WriteFile implements vfs.FS.
func (fs *CompilerFS) WriteFile(path string, data string, writeByteOrderMark bool) error {
	panic("unimplemented")
}

// Remove implements vfs.FS.
func (fs *CompilerFS) Remove(path string) error {
	panic("unimplemented")
}

// Chtimes implements vfs.FS.
func (fs *CompilerFS) Chtimes(path string, atime time.Time, mtime time.Time) error {
	panic("unimplemented")
}

func (c *compilerHost) MakeResolver(host module.ResolutionHost, options *core.CompilerOptions, typingsLocation string, projectName string) module.ResolverInterface {
	return module.NewResolver(host, options, typingsLocation, projectName)
}

func (c *compilerHost) Builder() *ProjectCollectionBuilder {
	return c.builder
}

func (c *compilerHost) SessionOptions() *SessionOptions {
	return c.sessionOptions
}

func (c *compilerHost) SeenFiles() *collections.SyncSet[tspath.Path] {
	return c.sourceFS.seenFiles
}

func (c *compilerHost) UpdateSeenFiles(seenFiles *collections.SyncSet[tspath.Path]) {
	c.sourceFS.seenFiles = seenFiles
}

func (c *compilerHost) Freeze(snapshotFS *SnapshotFS, configFileRegistry *ConfigFileRegistry) {
	c.freeze(snapshotFS, configFileRegistry)
}

func (c *compilerHost) CompilerFS() *CompilerFS {
	return c.compilerFS
}

func (c *compilerHost) SourceFS() *SourceFS {
	return c.sourceFS
}
