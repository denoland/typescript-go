package api

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-json-experiment/json"
	"github.com/microsoft/typescript-go/internal/ast"
	"github.com/microsoft/typescript-go/internal/bundled"
	"github.com/microsoft/typescript-go/internal/collections"
	"github.com/microsoft/typescript-go/internal/core"
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

//go:generate go tool golang.org/x/tools/cmd/stringer -type=MessageType -output=stringer_generated.go
//go:generate go tool mvdan.cc/gofumpt -w stringer_generated.go

type MessageType uint8

const (
	MessageTypeUnknown MessageType = iota
	MessageTypeRequest
	MessageTypeCallResponse
	MessageTypeCallError
	MessageTypeResponse
	MessageTypeError
	MessageTypeCall
)

func (m MessageType) IsValid() bool {
	return m >= MessageTypeRequest && m <= MessageTypeCall
}

type MessagePackType uint8

const (
	MessagePackTypeFixedArray3 MessagePackType = 0x93
	MessagePackTypeBin8        MessagePackType = 0xC4
	MessagePackTypeBin16       MessagePackType = 0xC5
	MessagePackTypeBin32       MessagePackType = 0xC6
	MessagePackTypeU8          MessagePackType = 0xCC
)

type Callback int

const (
	CallbackDirectoryExists Callback = 1 << iota
	CallbackFileExists
	CallbackGetAccessibleEntries
	CallbackReadFile
	CallbackRealpath
	CallbackResolveModuleName
	CallbackResolveTypeReferenceDirective
	CallbackGetPackageJsonScopeIfApplicable
	CallbackGetPackageScopeForPath
)

type ServerOptions struct {
	In                 io.Reader
	Out                io.Writer
	Err                io.Writer
	Cwd                string
	DefaultLibraryPath string
}

var _ vfs.FS = (*Server)(nil)

type Server struct {
	r      *bufio.Reader
	w      *bufio.Writer
	stderr io.Writer

	cwd                string
	newLine            string
	fs                 vfs.FS
	defaultLibraryPath string

	callbackMu       sync.Mutex
	enabledCallbacks Callback
	logger           logging.Logger
	api              *API

	requestId int
}

type hostWrapper struct {
	inner  project.ProjectHost
	server *Server
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
	return newResolverWrapper(h.inner.MakeResolver(host, options, typingsLocation, projectName), h.server)
}

// SeenFiles implements project.ProjectHost.
func (h *hostWrapper) SeenFiles() *collections.SyncSet[tspath.Path] {
	return h.inner.SeenFiles()
}

// Trace implements project.ProjectHost.
func (h *hostWrapper) Trace(msg string) {
	h.inner.Trace(msg)
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

func newProjectHostWrapper(currentDirectory string, proj *project.Project, builder *project.ProjectCollectionBuilder, logger *logging.LogTree, server *Server) *hostWrapper {
	inner := project.NewProjectHost(currentDirectory, proj, builder, logger)
	return &hostWrapper{
		inner:  inner,
		server: server,
	}
}

type resolverWrapper struct {
	inner  module.ResolverInterface
	server *Server
}

func newResolverWrapper(inner module.ResolverInterface, server *Server) *resolverWrapper {
	return &resolverWrapper{
		inner:  inner,
		server: server,
	}
}

type PackageJsonIfApplicable struct {
	PackageDirectory string
	DirectoryExists  bool
	Contents         string
}

// GetPackageJsonScopeIfApplicable implements module.ResolverInterface.
func (r *resolverWrapper) GetPackageJsonScopeIfApplicable(path string) *packagejson.InfoCacheEntry {
	if r.server.CallbackEnabled(CallbackGetPackageJsonScopeIfApplicable) {
		result, err := r.server.call("getPackageJsonScopeIfApplicable", path)
		if err != nil {
			panic(err)
		}
		if len(result) > 0 {
			var res PackageJsonIfApplicable
			if err := json.Unmarshal(result, &res); err != nil {
				panic(err)
			}
			contents, err := packagejson.Parse([]byte(res.Contents))
			if err != nil {
				panic(err)
			}
			return &packagejson.InfoCacheEntry{
				PackageDirectory: res.PackageDirectory,
				DirectoryExists:  res.DirectoryExists,
				Contents:         &packagejson.PackageJson{
					Fields: contents,
				},
			}
		} else {
			return nil
		}
	}
	return r.inner.GetPackageJsonScopeIfApplicable(path)
}

// GetPackageScopeForPath implements module.ResolverInterface.
func (r *resolverWrapper) GetPackageScopeForPath(directory string) *packagejson.InfoCacheEntry {
	if r.server.CallbackEnabled(CallbackGetPackageScopeForPath) {
		result, err := r.server.call("getPackageScopeForPath", directory)
		if err != nil {
			panic(err)
		}
		if len(result) > 0 {
			var res packagejson.InfoCacheEntry
			if err := json.Unmarshal(result, &res); err != nil {
				panic(err)
			}
			return &res
		}
	}
	return r.inner.GetPackageScopeForPath(directory)
}

// ResolveModuleName implements module.ResolverInterface.
func (r *resolverWrapper) ResolveModuleName(moduleName string, containingFile string, resolutionMode core.ResolutionMode, redirectedReference module.ResolvedProjectReference) (*module.ResolvedModule, []string) {
	if r.server.CallbackEnabled(CallbackResolveModuleName) {
		result, err := r.server.call("resolveModuleName", map[string]any{
			"moduleName": moduleName,
			"containingFile": containingFile,
			"resolutionMode": resolutionMode,
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
	return r.inner.ResolveModuleName(moduleName, containingFile, resolutionMode, redirectedReference)
}

// ResolveTypeReferenceDirective implements module.ResolverInterface.
func (r *resolverWrapper) ResolveTypeReferenceDirective(typeReferenceDirectiveName string, containingFile string, resolutionMode core.ResolutionMode, redirectedReference module.ResolvedProjectReference) (*module.ResolvedTypeReferenceDirective, []string) {
	if r.server.CallbackEnabled(CallbackResolveTypeReferenceDirective) {
		result, err := r.server.call("resolveTypeReferenceDirective", map[string]any{
			"typeReferenceDirectiveName": typeReferenceDirectiveName,
			"containingFile": containingFile,
			"resolutionMode": resolutionMode,
			"redirectedReference": redirectedReference,
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

var _ module.ResolverInterface = (*resolverWrapper)(nil)

func NewServer(options *ServerOptions) *Server {
	if options.Cwd == "" {
		panic("Cwd is required")
	}

	server := &Server{
		r:                  bufio.NewReader(options.In),
		w:                  bufio.NewWriter(options.Out),
		stderr:             options.Err,
		cwd:                options.Cwd,
		fs:                 bundled.WrapFS(osvfs.FS()),
		defaultLibraryPath: options.DefaultLibraryPath,
	}
	
	logger := logging.NewLogger(options.Err)
	// logger := NoLogger{}
	server.logger = logger
	server.api = NewAPI(&APIInit{
		Logger: logger,
		FS:     server,
		SessionOptions: &project.SessionOptions{
			CurrentDirectory:   options.Cwd,
			DefaultLibraryPath: options.DefaultLibraryPath,
			PositionEncoding:   lsproto.PositionEncodingKindUTF8,
			LoggingEnabled:     true,
			MakeHost: func(currentDirectory string, proj *project.Project, builder *project.ProjectCollectionBuilder, logger *logging.LogTree) project.ProjectHost {
				return newProjectHostWrapper(currentDirectory, proj, builder, logger, server)
			},
		},
	})
	return server
}

// DefaultLibraryPath implements APIHost.
func (s *Server) DefaultLibraryPath() string {
	return s.defaultLibraryPath
}

// FS implements APIHost.
func (s *Server) FS() vfs.FS {
	return s
}

// GetCurrentDirectory implements APIHost.
func (s *Server) GetCurrentDirectory() string {
	return s.cwd
}

func (s *Server) Run() error {
	for {
		messageType, method, payload, err := s.readRequest("")
		if err != nil {
			return err
		}

		switch messageType {
		case MessageTypeRequest:
			defer func() {
				if r := recover(); r != nil {
					stack := debug.Stack()
					err = fmt.Errorf("panic handling request: %v\n%s", r, string(stack))
					if fatalErr := s.sendError(method, err); fatalErr != nil {
						panic("fatal error sending panic response")
					}
				}
			}()

			result, err := s.handleRequest(method, payload)

			if err != nil {
				if err := s.sendError(method, err); err != nil {
					return err
				}
			} else {
				if err := s.sendResponse(method, result); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("%w: expected request, received: %s", ErrInvalidRequest, messageType.String())
		}
	}
}

func (s *Server) readRequest(expectedMethod string) (messageType MessageType, method string, payload []byte, err error) {
	t, err := s.r.ReadByte()
	if err != nil {
		return messageType, method, payload, err
	}
	if MessagePackType(t) != MessagePackTypeFixedArray3 {
		return messageType, method, payload, fmt.Errorf("%w: expected message to be encoded as fixed 3-element array (0x93), received: 0x%2x", ErrInvalidRequest, t)
	}
	t, err = s.r.ReadByte()
	if err != nil {
		return messageType, method, payload, err
	}
	if MessagePackType(t) != MessagePackTypeU8 {
		return messageType, method, payload, fmt.Errorf("%w: expected first element of message tuple to be encoded as unsigned 8-bit int (0xcc), received: 0x%2x", ErrInvalidRequest, t)
	}
	rawMessageType, err := s.r.ReadByte()
	if err != nil {
		return messageType, method, payload, err
	}
	messageType = MessageType(rawMessageType)
	if !messageType.IsValid() {
		return messageType, method, payload, fmt.Errorf("%w: unknown message type: %d", ErrInvalidRequest, messageType)
	}
	rawMethod, err := s.readBin()
	if err != nil {
		return messageType, method, payload, err
	}
	method = string(rawMethod)
	if expectedMethod != "" && method != expectedMethod {
		return messageType, method, payload, fmt.Errorf("%w: expected method %q, received %q", ErrInvalidRequest, expectedMethod, method)
	}
	payload, err = s.readBin()
	return messageType, method, payload, err
}

func (s *Server) readBin() ([]byte, error) {
	// https://github.com/msgpack/msgpack/blob/master/spec.md#bin-format-family
	t, err := s.r.ReadByte()
	if err != nil {
		return nil, err
	}
	var size uint
	switch MessagePackType(t) {
	case MessagePackTypeBin8:
		var size8 uint8
		if err = binary.Read(s.r, binary.BigEndian, &size8); err != nil {
			return nil, err
		}
		size = uint(size8)
	case MessagePackTypeBin16:
		var size16 uint16
		if err = binary.Read(s.r, binary.BigEndian, &size16); err != nil {
			return nil, err
		}
		size = uint(size16)
	case MessagePackTypeBin32:
		var size32 uint32
		if err = binary.Read(s.r, binary.BigEndian, &size32); err != nil {
			return nil, err
		}
		size = uint(size32)
	default:
		return nil, fmt.Errorf("%w: expected binary data length (0xc4-0xc6), received: 0x%2x", ErrInvalidRequest, t)
	}
	payload := make([]byte, size)
	bytesRead, err := io.ReadFull(s.r, payload)
	if err != nil {
		return nil, err
	}
	if bytesRead != int(size) {
		return nil, fmt.Errorf("%w: expected %d bytes, read %d", ErrInvalidRequest, size, bytesRead)
	}
	return payload, nil
}

func (s *Server) enableCallback(callback string) error {
	switch callback {
	case "directoryExists":
		s.enabledCallbacks |= CallbackDirectoryExists
	case "fileExists":
		s.enabledCallbacks |= CallbackFileExists
	case "getAccessibleEntries":
		s.enabledCallbacks |= CallbackGetAccessibleEntries
	case "readFile":
		s.enabledCallbacks |= CallbackReadFile
	case "realpath":
		s.enabledCallbacks |= CallbackRealpath
	case "resolveModuleName":
		s.enabledCallbacks |= CallbackResolveModuleName
	case "resolveTypeReferenceDirective":
		s.enabledCallbacks |= CallbackResolveTypeReferenceDirective
	case "getPackageJsonScopeIfApplicable":
		s.enabledCallbacks |= CallbackGetPackageJsonScopeIfApplicable
	case "getPackageScopeForPath":
		s.enabledCallbacks |= CallbackGetPackageScopeForPath
	default:
		return fmt.Errorf("unknown callback: %s", callback)
	}
	return nil
}

func (s *Server) handleRequest(method string, payload []byte) ([]byte, error) {
	s.requestId++
	switch method {
	case "configure":
		return nil, s.handleConfigure(payload)
	case "echo":
		return payload, nil
	default:
		return s.api.HandleRequest(core.WithRequestID(context.Background(), strconv.Itoa(s.requestId)), method, payload)
	}
}

func (s *Server) handleConfigure(payload []byte) error {
	var params *ConfigureParams
	if err := json.Unmarshal(payload, &params); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	for _, callback := range params.Callbacks {
		if err := s.enableCallback(callback); err != nil {
			return err
		}
	}
	// !!!
	if params.LogFile != "" {
		// s.logger.SetFile(params.LogFile)
	} else {
		// s.logger.SetFile("")
	}
	return nil
}

func (s *Server) sendResponse(method string, result []byte) error {
	return s.writeMessage(MessageTypeResponse, method, result)
}

func (s *Server) sendError(method string, err error) error {
	return s.writeMessage(MessageTypeError, method, []byte(err.Error()))
}

func (s *Server) writeMessage(messageType MessageType, method string, payload []byte) error {
	if err := s.w.WriteByte(byte(MessagePackTypeFixedArray3)); err != nil {
		return err
	}
	if err := s.w.WriteByte(byte(MessagePackTypeU8)); err != nil {
		return err
	}
	if err := s.w.WriteByte(byte(messageType)); err != nil {
		return err
	}
	if err := s.writeBin([]byte(method)); err != nil {
		return err
	}
	if err := s.writeBin(payload); err != nil {
		return err
	}
	return s.w.Flush()
}

func (s *Server) writeBin(payload []byte) error {
	length := len(payload)
	if length < 256 {
		if err := s.w.WriteByte(byte(MessagePackTypeBin8)); err != nil {
			return err
		}
		if err := s.w.WriteByte(byte(length)); err != nil {
			return err
		}
	} else if length < 1<<16 {
		if err := s.w.WriteByte(byte(MessagePackTypeBin16)); err != nil {
			return err
		}
		if err := binary.Write(s.w, binary.BigEndian, uint16(length)); err != nil {
			return err
		}
	} else {
		if err := s.w.WriteByte(byte(MessagePackTypeBin32)); err != nil {
			return err
		}
		if err := binary.Write(s.w, binary.BigEndian, uint32(length)); err != nil {
			return err
		}
	}
	_, err := s.w.Write(payload)
	return err
}

func (s *Server) call(method string, payload any) ([]byte, error) {
	s.callbackMu.Lock()
	defer s.callbackMu.Unlock()
	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if err = s.writeMessage(MessageTypeCall, method, jsonPayload); err != nil {
		return nil, err
	}

	messageType, _, responsePayload, err := s.readRequest(method)
	if err != nil {
		return nil, err
	}

	if messageType != MessageTypeCallResponse && messageType != MessageTypeCallError {
		return nil, fmt.Errorf("%w: expected call-response or call-error, received: %s", ErrInvalidRequest, messageType.String())
	}

	if messageType == MessageTypeCallError {
		return nil, fmt.Errorf("%w: %s", ErrClientError, responsePayload)
	}

	return responsePayload, nil
}

// DirectoryExists implements vfs.FS.
func (s *Server) DirectoryExists(path string) bool {
	if s.enabledCallbacks&CallbackDirectoryExists != 0 {
		result, err := s.call("directoryExists", path)
		if err != nil {
			panic(err)
		}
		if len(result) > 0 {
			return string(result) == "true"
		}
	}
	return s.fs.DirectoryExists(path)
}

// FileExists implements vfs.FS.
func (s *Server) FileExists(path string) bool {
	if s.enabledCallbacks&CallbackFileExists != 0 {
		result, err := s.call("fileExists", path)
		if err != nil {
			panic(err)
		}
		if len(result) > 0 {
			return string(result) == "true"
		}
	}
	return s.fs.FileExists(path)
}

// GetAccessibleEntries implements vfs.FS.
func (s *Server) GetAccessibleEntries(path string) vfs.Entries {
	if s.enabledCallbacks&CallbackGetAccessibleEntries != 0 {
		result, err := s.call("getAccessibleEntries", path)
		if err != nil {
			panic(err)
		}
		if len(result) > 0 {
			var rawEntries *struct {
				Files       []string `json:"files"`
				Directories []string `json:"directories"`
			}
			if err := json.Unmarshal(result, &rawEntries); err != nil {
				panic(err)
			}
			if rawEntries != nil {
				return vfs.Entries{
					Files:       rawEntries.Files,
					Directories: rawEntries.Directories,
				}
			}
		}
	}
	return s.fs.GetAccessibleEntries(path)
}

// ReadFile implements vfs.FS.
func (s *Server) ReadFile(path string) (contents string, ok bool) {
	if s.enabledCallbacks&CallbackReadFile != 0 && !strings.HasPrefix(path, "bundled://") {

		data, err := s.call("readFile", path)
		if err != nil {
			panic(err)
		}
		if string(data) == "null" {
			return "", false
		}
		if len(data) > 0 {
			var result string
			if err := json.Unmarshal(data, &result); err != nil {
				panic(err)
			}
			return result, true
		}
	}
	return s.fs.ReadFile(path)
}

// Realpath implements vfs.FS.
func (s *Server) Realpath(path string) string {
	if s.enabledCallbacks&CallbackRealpath != 0 {
		data, err := s.call("realpath", path)
		if err != nil {
			panic(err)
		}
		if len(data) > 0 {
			var result string
			if err := json.Unmarshal(data, &result); err != nil {
				panic(err)
			}
			return result
		}
	}
	return s.fs.Realpath(path)
}

// UseCaseSensitiveFileNames implements vfs.FS.
func (s *Server) UseCaseSensitiveFileNames() bool {
	return s.fs.UseCaseSensitiveFileNames()
}

// WriteFile implements vfs.FS.
func (s *Server) WriteFile(path string, data string, writeByteOrderMark bool) error {
	return s.fs.WriteFile(path, data, writeByteOrderMark)
}

// WalkDir implements vfs.FS.
func (s *Server) WalkDir(root string, walkFn vfs.WalkDirFunc) error {
	panic("unimplemented")
}

// Stat implements vfs.FS.
func (s *Server) Stat(path string) vfs.FileInfo {
	panic("unimplemented")
}

// Remove implements vfs.FS.
func (s *Server) Remove(path string) error {
	panic("unimplemented")
}

// Chtimes implements vfs.FS.
func (s *Server) Chtimes(path string, aTime time.Time, mTime time.Time) error {
	panic("unimplemented")
}

func (s *Server) CallbackEnabled(callback Callback) bool {
	return s.enabledCallbacks&callback != 0
}
