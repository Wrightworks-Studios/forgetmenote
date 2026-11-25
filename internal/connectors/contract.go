package connectors

import (
	"context"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// discoveryLifetime bounds how long calls trust an upstream's discovered
// contract without asking it again. Inspection through Tools always asks.
const discoveryLifetime = time.Minute

// contract is a connection's checked public operations with their resolved
// schemas. A configured connection computes it once; MCP and broker
// connections discover it from their upstream.
type contract struct {
	operations    []Operation
	input, output map[string]*jsonschema.Resolved
	// credential identifies the secret the upstream answered for, and expires
	// ends a discovered contract's reuse.
	credential string
	expires    time.Time
}

// newContract checks ops. Every operation needs an input schema that resolves;
// an output schema that does not resolve refuses that operation's results.
func newContract(ops []Operation) (*contract, error) {
	c := &contract{input: map[string]*jsonschema.Resolved{}, output: map[string]*jsonschema.Resolved{}}
	for _, op := range ops {
		op, err := checkedOperation(op)
		if err != nil {
			return nil, err
		}
		if c.input[op.Name], err = resolveSchema(op.InputSchema); err != nil {
			return nil, ErrInvalid
		}
		if len(op.OutputSchema) > 0 {
			c.output[op.Name], _ = resolveSchema(op.OutputSchema)
		}
		c.operations = append(c.operations, op)
	}
	return c, nil
}

func (c *contract) find(name string) *Operation {
	for i := range c.operations {
		if c.operations[i].Name == name {
			return &c.operations[i]
		}
	}
	return nil
}

// public returns copies, so callers cannot change the contract calls check.
func (c *contract) public() []Operation {
	out := make([]Operation, 0, len(c.operations))
	for _, op := range c.operations {
		out = append(out, cloneOperation(op))
	}
	return out
}

func discovers(kind string) bool { return kind == "mcp" || kind == "broker" }

// upstream is one request's view of a connection's remote end: the credential
// it read and, for MCP, the session opened on first use.
type upstream struct {
	*runtime
	secret  string
	session *mcp.ClientSession
}

func (u *upstream) mcpSession(ctx context.Context) (*mcp.ClientSession, error) {
	if u.session == nil {
		session, err := u.runtime.session(ctx, u.secret)
		if err != nil {
			return nil, err
		}
		u.session = session
	}
	return u.session, nil
}

func (u *upstream) close() {
	if u.session != nil {
		u.session.Close()
	}
}

// contract returns the configured contract, or the upstream's: reused when it
// was discovered for the same credential within discoveryLifetime, unless
// fresh asks again. cached reports a reused discovery.
func (u *upstream) contract(ctx context.Context, fresh bool) (c *contract, cached bool, err error) {
	if !discovers(u.config.Kind) {
		return u.configured, false, u.configuredErr
	}
	credential := hash(u.secret)
	u.contractMu.Lock()
	c = u.discovered
	u.contractMu.Unlock()
	if !fresh && c != nil && c.credential == credential && time.Now().Before(c.expires) {
		return c, true, nil
	}
	var ops []Operation
	if u.config.Kind == "mcp" {
		var session *mcp.ClientSession
		if session, err = u.mcpSession(ctx); err == nil {
			ops, err = u.sessionTools(ctx, session)
		}
	} else {
		ops, err = u.brokerTools(ctx, u.secret)
	}
	if err == nil {
		c, err = newContract(ops)
	}
	if err != nil {
		u.forgetContract()
		return nil, false, err
	}
	c.credential, c.expires = credential, time.Now().Add(discoveryLifetime)
	u.contractMu.Lock()
	u.discovered = c
	u.contractMu.Unlock()
	return c, false, nil
}

// forgetContract makes the next call ask the upstream again, after a failure
// that may mean its contract changed.
func (r *runtime) forgetContract() {
	r.contractMu.Lock()
	r.discovered = nil
	r.contractMu.Unlock()
}
