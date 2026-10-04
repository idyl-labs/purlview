// Copyright 2026 Idyl Labs
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"github.com/idyl-labs/purlview/internal/ipc"
	"github.com/idyl-labs/purlview/internal/share"
	"github.com/idyl-labs/purlview/internal/share/engine"
	"github.com/idyl-labs/purlview/internal/updatecheck"
)

// Timing limits of the server side; docs/daemon.md documents them.
const (
	// DefaultIdleExit is how long the daemon stays alive with no clients and
	// no owned operations before it exits on its own.
	DefaultIdleExit = 60 * time.Second
	// requestReadTimeout bounds a client's silence between messages; the
	// CLI keeps its connection open while attached, and there is no
	// keepalive, so it is generous.
	requestReadTimeout = 0 // no limit: attached clients are silent for a long time
	// responseWriteTimeout bounds one write to a client.
	responseWriteTimeout = 5 * time.Second
	// eventQueueSize bounds queued events per subscriber; a subscriber that
	// falls this far behind is disconnected rather than stalling the daemon.
	eventQueueSize = 128
	// shutdownGrace lets pending writes flush before connections close.
	shutdownGrace = 500 * time.Millisecond
	// maxUpdateWait caps how long one update.check request may wait.
	maxUpdateWait = 10 * time.Second
)

// Server is the daemon's protocol server and operation owner.
type Server struct {
	shares    *engine.Engine
	paths     Paths
	instance  string
	build     ipc.BuildIdentity
	exe       string
	exeSize   int64
	exeMod    time.Time
	startedAt time.Time
	idleExit  time.Duration
	testRes   bool
	logger    *log.Logger
	updates   *updatecheck.Checker

	mu           sync.Mutex
	conns        map[string]*serverConn
	shuttingDown bool
	shutdownWhy  string
	idleTimer    *time.Timer
	idleAt       time.Time
	ops          *registry
	stopCh       chan struct{}
	stopOnce     sync.Once
}

// ServerConfig configures a Server.
type ServerConfig struct {
	Shares        *engine.Engine
	Paths         Paths
	Instance      string
	Build         ipc.BuildIdentity
	Executable    string
	IdleExit      time.Duration
	TestResources bool
	Logger        *log.Logger
	Updates       *updatecheck.Checker
}

// NewServer prepares a server; Serve runs it.
func NewServer(cfg ServerConfig) (*Server, error) {
	s := &Server{
		shares:    cfg.Shares,
		paths:     cfg.Paths,
		instance:  cfg.Instance,
		build:     cfg.Build,
		exe:       cfg.Executable,
		startedAt: time.Now(),
		idleExit:  cfg.IdleExit,
		testRes:   cfg.TestResources,
		logger:    cfg.Logger,
		updates:   cfg.Updates,
		conns:     map[string]*serverConn{},
		stopCh:    make(chan struct{}),
	}
	if s.idleExit <= 0 {
		s.idleExit = DefaultIdleExit
	}
	if s.logger == nil {
		s.logger = log.New(io.Discard, "", 0)
	}
	if st, err := os.Stat(s.exe); err == nil {
		s.exeSize, s.exeMod = st.Size(), st.ModTime()
	}
	s.ops = newRegistry(s.onOperationEnded)
	return s, nil
}

// Instance returns the daemon instance id.
func (s *Server) Instance() string { return s.instance }

// Serve accepts connections until Shutdown is called or ctx is done, then
// ends every operation, notifies subscribers and closes connections. It
// returns the shutdown reason.
func (s *Server) Serve(ctx context.Context, l net.Listener) string {
	s.armIdle()
	go func() {
		select {
		case <-ctx.Done():
			s.Shutdown("signal")
		case <-s.stopCh:
		}
	}()
	go func() {
		<-s.stopCh
		_ = l.Close()
	}()
	for {
		c, err := l.Accept()
		if err != nil {
			select {
			case <-s.stopCh:
				return s.finish()
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return s.finish()
			}
			s.logger.Printf("accept: %v", err)
			continue
		}
		go s.handle(c)
	}
}

// Shutdown starts an orderly exit exactly once.
func (s *Server) Shutdown(reason string) {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.shuttingDown = true
		s.shutdownWhy = reason
		if s.idleTimer != nil {
			s.idleTimer.Stop()
		}
		s.mu.Unlock()
		close(s.stopCh)
	})
}

// finish runs after the listener closed: end work, notify, close clients.
func (s *Server) finish() string {
	if s.shares != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		s.shares.Shutdown(ctx)
		cancel()
	}
	n := s.ops.endAll(ReasonShutdown)
	s.mu.Lock()
	reason := s.shutdownWhy
	conns := make([]*serverConn, 0, len(s.conns))
	for _, c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	s.logger.Printf("shutdown (%s): ended %d operation(s), closing %d connection(s)", reason, n, len(conns))
	data := ipc.Marshal(map[string]string{"reason": reason})
	for _, c := range conns {
		c.enqueueEvent(&ipc.Event{Event: ipc.EventDaemonShutdown, Data: data})
	}
	if len(conns) > 0 {
		time.Sleep(shutdownGrace)
	}
	for _, c := range conns {
		c.close()
	}
	return reason
}

// armIdle (re)starts the idle timer when nothing keeps the daemon alive.
func (s *Server) armIdle() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.shuttingDown {
		return
	}
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
		s.idleAt = time.Time{}
	}
	if len(s.conns) > 0 || s.ops.activeCount() > 0 || (s.shares != nil && s.shares.Active() > 0) {
		return
	}
	s.idleAt = time.Now().Add(s.idleExit)
	s.idleTimer = time.AfterFunc(s.idleExit, func() {
		s.mu.Lock()
		idle := len(s.conns) == 0 && s.ops.activeCount() == 0 && (s.shares == nil || s.shares.Active() == 0)
		s.mu.Unlock()
		if idle {
			s.logger.Printf("idle for %s with no clients or operations; exiting", s.idleExit)
			s.Shutdown("idle")
		}
	})
}

func (s *Server) onOperationEnded(op ipc.Operation) {
	s.logger.Printf("operation %s (%s, %s) ended: %s", op.ID, op.Name, op.Owner, op.Reason)
	s.broadcast(&ipc.Event{Event: ipc.EventOpEnded, Data: ipc.Marshal(ipc.OpEndedData{Operation: op})})
	s.armIdle()
}

func (s *Server) broadcast(ev *ipc.Event) {
	s.mu.Lock()
	subs := make([]*serverConn, 0, len(s.conns))
	for _, c := range s.conns {
		if c.subscribed() {
			subs = append(subs, c)
		}
	}
	s.mu.Unlock()
	for _, c := range subs {
		c.enqueueEvent(ev)
	}
}

// serverConn is one client connection.
type serverConn struct {
	shares share.Session
	id     string
	srv    *Server
	conn   *ipc.Conn
	out    chan outbound
	closed chan struct{}
	once   sync.Once

	mu     sync.Mutex
	hello  *ipc.HelloParams
	protOK bool
	subbed bool
}

type outbound struct {
	resp *ipc.Response
	ev   *ipc.Event
}

func (c *serverConn) subscribed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.subbed
}

func (c *serverConn) close() {
	c.once.Do(func() {
		close(c.closed)
		_ = c.conn.Close()
	})
}

// enqueueEvent never blocks: a full queue disconnects the subscriber.
func (c *serverConn) enqueueEvent(ev *ipc.Event) {
	select {
	case c.out <- outbound{ev: ev}:
	default:
		c.srv.logger.Printf("client %s: event queue full; disconnecting slow subscriber", c.id)
		// Best effort: tell the client why, then close.
		go func() {
			_ = c.conn.WriteEvent(&ipc.Event{Event: ipc.EventOverflow}, time.Now().Add(time.Second))
			c.close()
		}()
	}
}

func (c *serverConn) writeLoop() {
	for {
		select {
		case <-c.closed:
			return
		case m := <-c.out:
			var err error
			if m.resp != nil {
				err = c.conn.WriteResponse(m.resp, time.Now().Add(responseWriteTimeout))
			} else {
				err = c.conn.WriteEvent(m.ev, time.Now().Add(responseWriteTimeout))
			}
			if err != nil {
				c.close()
				return
			}
		}
	}
}

func (s *Server) handle(raw net.Conn) {
	c := &serverConn{id: newID(), srv: s, conn: ipc.NewConn(raw), out: make(chan outbound, eventQueueSize), closed: make(chan struct{})}
	s.mu.Lock()
	if s.shuttingDown {
		s.mu.Unlock()
		_ = raw.Close()
		return
	}
	s.conns[c.id] = c
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
		s.idleAt = time.Time{}
	}
	s.mu.Unlock()
	go c.writeLoop()
	defer func() {
		if c.shares != nil {
			_ = c.shares.Close()
		}
		c.close()
		s.mu.Lock()
		delete(s.conns, c.id)
		s.mu.Unlock()
		n := s.ops.endOwnedBy(c.id, ReasonDisconnected)
		if n > 0 {
			s.logger.Printf("client %s disconnected; ended %d attached operation(s)", c.id, n)
		}
		s.armIdle()
	}()

	var deadline time.Time
	for {
		if requestReadTimeout > 0 {
			deadline = time.Now().Add(requestReadTimeout)
		}
		req, err := c.conn.ReadRequest(deadline)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				s.logger.Printf("client %s: %v", c.id, err)
			}
			return
		}
		resp := s.dispatch(c, req)
		select {
		case c.out <- outbound{resp: resp}:
		case <-c.closed:
			return
		}
		if req.Op == ipc.OpShutdown && resp.OK {
			// Let the acknowledgement reach the client, then exit.
			go func() {
				time.Sleep(50 * time.Millisecond)
				s.Shutdown("requested")
			}()
		}
	}
}

func fail(id, code, msg string) *ipc.Response {
	return &ipc.Response{ID: id, OK: false, Error: &ipc.Error{Code: code, Message: msg}}
}

func ok(id string, result any) *ipc.Response {
	r := &ipc.Response{ID: id, OK: true}
	if result != nil {
		r.Result = ipc.Marshal(result)
	}
	return r
}

func decode[T any](req *ipc.Request) (T, error) {
	var v T
	if len(req.Params) == 0 {
		return v, nil
	}
	err := json.Unmarshal(req.Params, &v)
	return v, err
}

func (s *Server) dispatch(c *serverConn, req *ipc.Request) *ipc.Response {
	// hello, status and shutdown are answered for any protocol version so a
	// newer CLI can identify and retire an older daemon.
	switch req.Op {
	case ipc.OpHello:
		p, err := decode[ipc.HelloParams](req)
		if err != nil {
			return fail(req.ID, ipc.CodeBadRequest, err.Error())
		}
		c.mu.Lock()
		c.hello = &p
		c.protOK = p.Protocol == ipc.ProtocolVersion
		c.mu.Unlock()
		res := ipc.HelloResult{
			Protocol: ipc.ProtocolVersion, Instance: s.instance, PID: os.Getpid(),
			StartedAt: s.startedAt.UTC().Format(time.RFC3339Nano), Build: s.build,
			Executable: s.exe, ExeSize: s.exeSize, ExeModTime: s.exeMod.UTC().Format(time.RFC3339Nano),
			Ready: true, TestResources: s.testRes, Console: consoleState(),
		}
		if p.Protocol != ipc.ProtocolVersion {
			r := fail(req.ID, ipc.CodeProtocolMismatch, fmt.Sprintf("daemon speaks protocol %d, client protocol %d", ipc.ProtocolVersion, p.Protocol))
			r.Result = ipc.Marshal(res)
			return r
		}
		return ok(req.ID, res)
	case ipc.OpStatus:
		return ok(req.ID, s.status())
	case ipc.OpShutdown:
		s.mu.Lock()
		down := s.shuttingDown
		s.mu.Unlock()
		if down {
			return fail(req.ID, ipc.CodeShuttingDown, "daemon is already shutting down")
		}
		return ok(req.ID, ipc.ShutdownResult{Operations: s.ops.activeCount()})
	}

	c.mu.Lock()
	hello, protOK := c.hello, c.protOK
	c.mu.Unlock()
	if hello == nil {
		return fail(req.ID, ipc.CodeBadRequest, "hello must be the first request")
	}
	if !protOK {
		return fail(req.ID, ipc.CodeProtocolMismatch, "protocol version mismatch; only status and shutdown are available")
	}
	s.mu.Lock()
	down := s.shuttingDown
	s.mu.Unlock()
	if down {
		return fail(req.ID, ipc.CodeShuttingDown, "daemon is shutting down")
	}

	switch req.Op {
	case ipc.OpSubscribe:
		c.mu.Lock()
		c.subbed = true
		c.mu.Unlock()
		return ok(req.ID, nil)
	case ipc.OpUpdateCheck:
		p, err := decode[ipc.UpdateCheckParams](req)
		if err != nil {
			return fail(req.ID, ipc.CodeBadRequest, err.Error())
		}
		if s.updates == nil {
			return ok(req.ID, ipc.UpdateCheckResult{Status: "none", Outcome: "disabled"})
		}
		wait := time.Duration(p.WaitMs) * time.Millisecond
		if wait < 0 {
			wait = 0
		}
		if wait > maxUpdateWait {
			wait = maxUpdateWait
		}
		return ok(req.ID, s.updates.Check(context.Background(), p.Channel, wait))
	case ipc.OpShareStart, ipc.OpShareStop, ipc.OpShareStopAll, ipc.OpShareList:
		if s.shares != nil {
			return s.dispatchShare(c, req)
		}
		// This daemon has no platform configured, so it answers honestly:
		// nothing was created, stopped or listed.
		return fail(req.ID, ipc.CodeNotImplemented, "sharing is not implemented in this scaffold build; no share was created")
	case ipc.OpTestStart, ipc.OpTestStop, ipc.OpTestList, ipc.OpTestRevoke:
		if !s.testRes {
			return fail(req.ID, ipc.CodeNotImplemented, "sharing is not implemented in this scaffold build and test resources are not enabled on this daemon")
		}
		return s.dispatchTest(c, req)
	}
	return fail(req.ID, ipc.CodeUnknownOp, fmt.Sprintf("unknown operation %q", req.Op))
}

func (s *Server) dispatchTest(c *serverConn, req *ipc.Request) *ipc.Response {
	switch req.Op {
	case ipc.OpTestStart:
		p, err := decode[ipc.TestStartParams](req)
		if err != nil {
			return fail(req.ID, ipc.CodeBadRequest, err.Error())
		}
		if p.Name == "" {
			p.Name = "test"
		}
		op := s.ops.start(p.Name, p.Detached, c.id, time.Duration(p.TTLMs)*time.Millisecond)
		s.logger.Printf("operation %s (%s, %s) started by client %s", op.ID, op.Name, op.Owner, c.id)
		s.broadcast(&ipc.Event{Event: ipc.EventOpStarted, Data: ipc.Marshal(ipc.OpEndedData{Operation: op})})
		return ok(req.ID, op)
	case ipc.OpTestStop, ipc.OpTestRevoke:
		p, err := decode[ipc.TestStopParams](req)
		if err != nil {
			return fail(req.ID, ipc.CodeBadRequest, err.Error())
		}
		reason := ReasonStopped
		if req.Op == ipc.OpTestRevoke {
			reason = ReasonRevoked
		}
		op, found, first := s.ops.end(p.ID, reason)
		if !found {
			return fail(req.ID, ipc.CodeNotFound, fmt.Sprintf("operation %q not found", p.ID))
		}
		return ok(req.ID, ipc.TestStopResult{Operation: op, AlreadyEnded: !first})
	case ipc.OpTestList:
		return ok(req.ID, ipc.TestListResult{Operations: s.ops.list()})
	}
	return fail(req.ID, ipc.CodeUnknownOp, req.Op)
}

func (s *Server) status() ipc.StatusResult {
	s.mu.Lock()
	res := ipc.StatusResult{
		Instance: s.instance, PID: os.Getpid(), StartedAt: s.startedAt.UTC().Format(time.RFC3339Nano),
		UptimeSeconds: time.Since(s.startedAt).Seconds(), Clients: len(s.conns), ShuttingDown: s.shuttingDown,
	}
	if !s.idleAt.IsZero() {
		res.IdleExitAt = s.idleAt.UTC().Format(time.RFC3339Nano)
	}
	s.mu.Unlock()
	res.Operations = s.ops.list()
	if res.Operations == nil {
		res.Operations = []ipc.Operation{}
	}
	return res
}
