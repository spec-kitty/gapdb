package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spec-kitty/gapdb/gapdb"
	"github.com/spec-kitty/gapdb/internal/faultfs"
	"github.com/spec-kitty/gapdb/internal/owner"
	"github.com/spec-kitty/gapdb/internal/persist"
	"github.com/spec-kitty/gapdb/internal/protocol"
)

type Config struct {
	Directory               string
	SocketPath              string
	Options                 gapdb.Options
	ToolVersion             string
	QueueCapacity           int
	ReadTimeout             time.Duration
	WriteTimeout            time.Duration
	IdleTimeout             time.Duration
	FS                      faultfs.FS
	BeforeLockModeCheck     func()
	AfterStaleSocketCleanup func()
	AfterListenerBind       func()
}

type Server struct {
	directory  string
	socket     string
	limits     gapdb.Limits
	runtime    *owner.Runtime
	wal        *persist.WAL
	fs         faultfs.FS
	listener   *net.UnixListener
	socketDir  *directoryAnchor
	socketName string
	read       time.Duration
	write      time.Duration
	idle       time.Duration
	clients    chan struct{}
	shutdown   chan struct{}
	draining   atomic.Bool
	manifest   atomic.Uint64
	requests   atomic.Uint64
	startedAt  time.Time

	mu        sync.Mutex
	conns     map[net.Conn]struct{}
	wg        sync.WaitGroup
	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error
}

func Open(config Config) (*Server, error) {
	if config.Directory == "" {
		return nil, invalidRequest("directory", "must not be empty")
	}
	config.Directory = filepath.Clean(config.Directory)
	if config.Options == (gapdb.Options{}) {
		config.Options = gapdb.DefaultOptions()
	}
	if config.FS == nil {
		config.FS = faultfs.NewOS(nil)
	}
	if err := config.Options.Validate(); err != nil {
		return nil, err
	}
	if config.ToolVersion == "" {
		return nil, invalidRequest("tool_version", "must not be empty")
	}
	if config.SocketPath == "" {
		config.SocketPath = filepath.Join(config.Directory, "gapdb.sock")
	}
	config.SocketPath = filepath.Clean(config.SocketPath)
	socketParent := filepath.Dir(config.SocketPath)
	var socketDir *directoryAnchor
	var err error
	if socketParent != config.Directory {
		socketDir, err = openDirectoryAnchor(socketParent)
		if err != nil {
			return nil, permissionError(config.SocketPath, "socket_directory_open", err)
		}
	}
	opened, err := openRuntime(config)
	if err != nil {
		if socketDir != nil {
			_ = socketDir.close()
		}
		return nil, err
	}
	fail := func(err error) (*Server, error) {
		if socketDir != nil {
			_ = socketDir.close()
		}
		_ = opened.runtime.Close(context.Background())
		_ = opened.wal.Close()
		return nil, err
	}
	if socketDir == nil {
		socketDir, err = openDirectoryAnchor(socketParent)
		if err != nil {
			return fail(permissionError(config.SocketPath, "socket_directory_open", err))
		}
	}
	socketName := filepath.Base(config.SocketPath)
	if err := secureHeldLock(opened.runtime, config.Directory); err != nil {
		return fail(err)
	}
	if err := removeStaleSocket(socketDir, socketName); err != nil {
		return fail(err)
	}
	if config.AfterStaleSocketCleanup != nil {
		config.AfterStaleSocketCleanup()
	}
	if err := secureHeldLock(opened.runtime, config.Directory); err != nil {
		return fail(err)
	}
	if err := socketDir.validatePath(); err != nil {
		return fail(permissionError(config.SocketPath, "socket_directory_identity", err))
	}
	bindPath, err := socketDir.bindPath(socketName)
	if err != nil {
		return fail(permissionError(config.SocketPath, "socket_bind_path", err))
	}
	address := &net.UnixAddr{Name: bindPath, Net: "unix"}
	listener, err := net.ListenUnix("unix", address)
	if err != nil {
		return fail(permissionError(config.SocketPath, "listen", err))
	}
	listener.SetUnlinkOnClose(false)
	socketIdentity, err := socketDir.secureSocket(socketName)
	if err != nil {
		_ = listener.Close()
		_ = socketDir.removeSocket(socketName)
		return fail(permissionError(config.SocketPath, "socket_mode", err))
	}
	if config.AfterListenerBind != nil {
		config.AfterListenerBind()
	}
	if err := secureHeldLock(opened.runtime, config.Directory); err != nil {
		_ = listener.Close()
		_ = socketDir.removeSocket(socketName)
		return fail(err)
	}
	if err := socketDir.verifySocket(socketName, socketIdentity); err != nil {
		_ = listener.Close()
		_ = socketDir.removeSocket(socketName)
		return fail(permissionError(config.SocketPath, "verify_mode", err))
	}
	server := &Server{directory: config.Directory, socket: config.SocketPath, limits: config.Options.Limits, runtime: opened.runtime, wal: opened.wal, fs: config.FS, listener: listener, socketDir: socketDir, socketName: socketName, read: config.ReadTimeout, write: config.WriteTimeout, idle: config.IdleTimeout, clients: make(chan struct{}, config.Options.Limits.MaxConcurrentClients), shutdown: make(chan struct{}), conns: make(map[net.Conn]struct{}), closeDone: make(chan struct{}), startedAt: time.Now().UTC()}
	server.manifest.Store(opened.manifestGeneration)
	server.wg.Add(1)
	go server.accept()
	return server, nil
}

func (server *Server) SocketPath() string { return server.socket }

func (server *Server) Close(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	server.closeOnce.Do(func() {
		server.draining.Store(true)
		close(server.shutdown)
		_ = server.listener.Close()
		server.mu.Lock()
		for conn := range server.conns {
			_ = conn.SetReadDeadline(time.Now())
		}
		server.mu.Unlock()
		go func() {
			server.wg.Wait()
			removeErr := server.socketDir.removeSocket(server.socketName)
			server.closeErr = errors.Join(removeErr, server.socketDir.close(), server.runtime.Close(context.Background()), server.wal.Close())
			close(server.closeDone)
		}()
	})
	select {
	case <-server.closeDone:
		return server.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (server *Server) accept() {
	defer server.wg.Done()
	for {
		conn, err := server.listener.AcceptUnix()
		if err != nil {
			if server.draining.Load() || errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		select {
		case server.clients <- struct{}{}:
			server.track(conn, true)
			server.wg.Add(1)
			go server.serveConnection(conn)
		default:
			active, maximum, depth := len(server.clients), cap(server.clients), server.runtime.Status().QueueDepth
			structured := &gapdb.Error{Code: gapdb.CodeServerBusy, Message: "The configured client limit is active.", Retry: gapdb.RetryImmediate, ActiveClients: &active, MaximumClients: &maximum, QueueDepth: &depth, SafeActions: []gapdb.SafeAction{gapdb.ActionRetryWithBackoff, gapdb.ActionAbort}}
			_ = server.writeResponse(conn, protocol.Response{SchemaVersion: protocol.SchemaVersion, OK: false, DatabaseID: server.runtime.Status().Status.DatabaseID, Operation: protocol.OperationStatus, Error: structured})
			_ = conn.Close()
		}
	}
}

func (server *Server) serveConnection(conn net.Conn) {
	defer server.wg.Done()
	defer func() {
		server.track(conn, false)
		<-server.clients
		_ = conn.Close()
	}()
	defer func() {
		if recovered := recover(); recovered != nil {
			server.runtime.State().MarkAdminDegraded()
			failure := &gapdb.Error{Code: gapdb.CodeInternal, Message: "A server handler failed safely.", Retry: gapdb.RetryAfterOperator, CorrelationID: fmt.Sprintf("panic-%d", server.requests.Add(1)), Lifecycle: gapdb.LifecycleDegradedReadOnly, SafeActions: []gapdb.SafeAction{gapdb.ActionStatus, gapdb.ActionVerify, gapdb.ActionAbort}}
			_ = server.writeResponse(conn, protocol.Response{SchemaVersion: protocol.SchemaVersion, OK: false, DatabaseID: server.runtime.Status().Status.DatabaseID, Operation: protocol.OperationStatus, Error: failure})
		}
	}()
	for {
		if server.draining.Load() {
			return
		}
		server.setReadDeadline(conn)
		payload, err := protocol.ReadFrame(conn, server.limits.MaxFrameBytes)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return
			}
			_ = server.writeFailure(conn, protocol.OperationStatus, "", err)
			return
		}
		request, err := protocol.DecodeRequest(payload, server.limits)
		if err != nil {
			_ = server.writeFailure(conn, protocol.OperationStatus, "", err)
			continue
		}
		if request.Operation == protocol.OperationWatch {
			server.handleWatch(conn, request)
			return
		}
		if request.Operation == protocol.OperationReadRecoverySnapshot {
			if err := server.handleRecoverySnapshot(conn, request); err != nil {
				return
			}
			continue
		}
		response := server.dispatch(request)
		if err := server.writeResponse(conn, response); err != nil {
			return
		}
	}
}

func (server *Server) track(conn net.Conn, add bool) {
	server.mu.Lock()
	if add {
		server.conns[conn] = struct{}{}
	} else {
		delete(server.conns, conn)
	}
	server.mu.Unlock()
}

func (server *Server) setReadDeadline(conn net.Conn) {
	timeout := server.idle
	if timeout <= 0 {
		timeout = server.read
	}
	if timeout > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(timeout))
	} else {
		_ = conn.SetReadDeadline(time.Time{})
	}
}

func (server *Server) writeResponse(conn net.Conn, response protocol.Response) error {
	if server.write > 0 {
		_ = conn.SetWriteDeadline(time.Now().Add(server.write))
	}
	payload, err := server.encodeResponseForFrame(response)
	if err != nil {
		return err
	}
	if err := faultfs.Checkpoint(server.fs, faultfs.PointResponsePublish, faultfs.Before); err != nil {
		return err
	}
	if err := protocol.WriteFrame(conn, payload, server.limits.MaxFrameBytes); err != nil {
		return err
	}
	return faultfs.Checkpoint(server.fs, faultfs.PointResponsePublish, faultfs.After)
}

func (server *Server) encodeResponseForFrame(response protocol.Response) ([]byte, error) {
	payload, err := protocol.EncodeResponse(response)
	receivedBytes := len(payload)
	if err != nil {
		var oversized *gapdb.Error
		if !errors.As(err, &oversized) || oversized.Code != gapdb.CodeFrameTooLarge {
			return nil, err
		}
		receivedBytes = oversized.ReceivedBytes
	}
	if err != nil || receivedBytes > server.limits.MaxFrameBytes {
		// Frame admission belongs at the fully correlated wire boundary. A
		// result-only estimate cannot safely account for JSON escaping in the
		// echoed request ID or for the rest of the response envelope. This path
		// also handles EncodeResponse rejecting a payload at the hard ceiling.
		failure := &gapdb.Error{
			Code:          gapdb.CodeFrameTooLarge,
			Message:       "Protocol response exceeds the configured frame limit.",
			Retry:         gapdb.RetryNever,
			ReceivedBytes: receivedBytes,
			MaximumBytes:  server.limits.MaxFrameBytes,
			SafeActions:   []gapdb.SafeAction{gapdb.ActionReduceRequest, gapdb.ActionAbort},
		}
		payload, err = protocol.EncodeResponse(protocol.Response{
			SchemaVersion: protocol.SchemaVersion,
			OK:            false,
			RequestID:     response.RequestID,
			DatabaseID:    response.DatabaseID,
			Operation:     response.Operation,
			Error:         failure,
		})
		if err != nil {
			return nil, err
		}
		if len(payload) > server.limits.MaxFrameBytes {
			return nil, failure
		}
	}
	return payload, nil
}

func (server *Server) writeFailure(conn net.Conn, operation protocol.Operation, requestID string, err error) error {
	return server.writeResponse(conn, protocol.Response{SchemaVersion: protocol.SchemaVersion, OK: false, RequestID: requestID, DatabaseID: server.runtime.Status().Status.DatabaseID, Operation: operation, Error: structuredError(err, operation, server.runtime.Status().Status)})
}

func removeStaleSocket(anchor *directoryAnchor, name string) error {
	if err := anchor.validatePath(); err != nil {
		return permissionError(name, "socket_directory_identity", err)
	}
	if err := anchor.removeSocket(name); err != nil {
		return permissionError(name, "remove_stale_socket", err)
	}
	return nil
}

func invalidRequest(field, reason string) error {
	return &gapdb.Error{Code: gapdb.CodeInvalidRequest, Message: "Request validation failed.", Retry: gapdb.RetryNever, Field: field, Reason: reason, SafeActions: []gapdb.SafeAction{gapdb.ActionFixRequest, gapdb.ActionAbort}}
}
