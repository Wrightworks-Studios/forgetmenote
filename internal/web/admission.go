package web

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"forgetmenote/internal/capacity"
)

// The SDK's legacy sessions retain initialization contexts. RequestExtra.Header,
// rather than that context, binds a message to its actual HTTP lifetime. This
// header is replaced by the server and carries no authentication authority.
const rpcLifetimeHeader = "X-ForgetMeNote-RPC-Lifetime"

type rpcHTTPKey struct{}
type rpcHTTPRegistry struct {
	mu       sync.Mutex
	next     uint64
	requests map[string]*rpcHTTPLifetime
}
type rpcHTTPLifetime struct {
	mu       sync.Mutex
	closed   bool
	decoding *rpcPermit
	calls    []*rpcPermit
}

// A permit survives both its work and its HTTP response. Disconnecting a legacy
// POST must not free capacity while the SDK is still executing its tool.
type rpcPermit struct {
	mu                     sync.Mutex
	work                   chan struct{}
	callDone, responseDone bool
}

func (p *rpcPermit) finish(call bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if call {
		p.callDone = true
	} else {
		p.responseDone = true
	}
	if p.callDone && p.responseDone && p.work != nil {
		<-p.work
		p.work = nil
	}
}
func (l *rpcHTTPLifetime) admit(work chan struct{}) (func(), error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, context.Canceled
	}
	if work == nil {
		if l.decoding != nil {
			l.decoding.finish(true)
		}
		return func() {}, nil
	}
	select {
	case work <- struct{}{}:
		permit := &rpcPermit{work: work}
		l.calls = append(l.calls, permit)
		if l.decoding != nil {
			l.decoding.finish(true)
		}
		return func() { permit.finish(true) }, nil
	default:
		return nil, capacity.ErrBusy
	}
}
func (l *rpcHTTPLifetime) isClosed() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}
func (l *rpcHTTPLifetime) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	if l.decoding != nil {
		l.decoding.finish(true)
	}
	for _, permit := range l.calls {
		permit.finish(false)
	}
}
func (r *rpcHTTPRegistry) register(l *rpcHTTPLifetime) (string, func()) {
	r.mu.Lock()
	r.next++
	id := fmt.Sprint(r.next)
	if r.requests == nil {
		r.requests = map[string]*rpcHTTPLifetime{}
	}
	r.requests[id] = l
	r.mu.Unlock()
	return id, func() {
		r.mu.Lock()
		delete(r.requests, id)
		r.mu.Unlock()
		l.close()
	}
}
func (r *rpcHTTPRegistry) lookup(header http.Header) *rpcHTTPLifetime {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requests[header.Get(rpcLifetimeHeader)]
}

// MCP's typed adapter encodes and validates output after the operation returns.
// Own that work through the complete receiving method, including disconnects.
type rpcMethodKey struct{}
type rpcMethodWork struct {
	mu          sync.Mutex
	closed      bool
	completions []func()
}

func (m *rpcMethodWork) retain(finish func()) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return false
	}
	m.completions = append(m.completions, finish)
	return true
}
func (m *rpcMethodWork) finish() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	completions := m.completions
	m.completions = nil
	m.mu.Unlock()
	for _, finish := range completions {
		finish()
	}
}
func admitRPC(ctx context.Context, work chan struct{}) (func(), error) {
	var release func()
	if lifetime, _ := ctx.Value(rpcHTTPKey{}).(*rpcHTTPLifetime); lifetime != nil {
		var err error
		release, err = lifetime.admit(work)
		if err != nil {
			return nil, err
		}
	} else if work == nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		release = func() {}
	} else {
		select {
		case work <- struct{}{}:
			release = func() { <-work }
		default:
			return nil, capacity.ErrBusy
		}
	}
	if method, _ := ctx.Value(rpcMethodKey{}).(*rpcMethodWork); method != nil {
		if !method.retain(release) {
			release()
			return nil, context.Canceled
		}
		return func() {}, nil
	}
	return release, nil
}
