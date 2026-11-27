// Package connectors owns operator-configured external data boundaries. It never
// accepts destinations or credentials from notebook content or tool arguments.
package connectors

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"forgetmenote/internal/durable"
	"forgetmenote/internal/integrations"
	"forgetmenote/internal/jsonfile"
	"maps"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var ErrUnavailable = errors.New("data connection unavailable")
var ErrInvalid = errors.New("invalid data operation")
var ErrSchemaChanged = errors.New("operation schema changed; inspect it again")

type Config struct {
	Version     int          `json:"version"`
	Connections []Connection `json:"connections"`
}
type Connection struct {
	personalToken          func(context.Context) (string, []string, error)
	Email                  *EmailConfig            `json:"email,omitempty"`
	Sheets                 *SheetsConfig           `json:"sheets,omitempty"`
	Calendar               *CalendarConfig         `json:"calendar,omitempty"`
	Projects               []string                `json:"projects,omitempty"`
	OAuthClientCredentials *OAuthClientCredentials `json:"oauthClientCredentials,omitempty"`
	OAuthRefresh           *OAuthRefresh           `json:"oauthRefresh,omitempty"`
	GitHubApp              *GitHubApp              `json:"githubApp,omitempty"`
	ID                     string                  `json:"id"`
	Label                  string                  `json:"label"`
	Kind                   string                  `json:"kind"`
	Endpoint               string                  `json:"endpoint"`
	HostHeader             string                  `json:"hostHeader,omitempty"`
	AllowClusterHTTP       bool                    `json:"allowClusterHTTP,omitempty"`
	SecretFile             string                  `json:"secretFile,omitempty"`
	SecretEnv              string                  `json:"secretEnv,omitempty"`
	UsernameFile           string                  `json:"usernameFile,omitempty"`
	UsernameEnv            string                  `json:"usernameEnv,omitempty"`
	AWS                    *HTTPSigning            `json:"aws,omitempty"`
	Login                  *HTTPLogin              `json:"login,omitempty"`
	AuthHeader             string                  `json:"authHeader,omitempty"`
	AuthScheme             string                  `json:"authScheme,omitempty"`
	CAFile                 string                  `json:"caFile,omitempty"`
	ExternalProcessing     bool                    `json:"externalProcessing,omitempty"`
	CodeOnly               bool                    `json:"codeOnly,omitempty"`
	TimeoutSeconds         int                     `json:"timeoutSeconds,omitempty"`
	MaxBytes               int                     `json:"maxBytes,omitempty"`
	MaxRows                int                     `json:"maxRows,omitempty"`
	Concurrent             int                     `json:"concurrent,omitempty"` // Zero forwards calls independently.
	Operations             []Operation             `json:"operations"`
	Redshift               *RedshiftConfig         `json:"redshift,omitempty"`
	Broker                 *BrokerConfig           `json:"broker,omitempty"`
}
type Operation struct {
	ExecutionDiagnostics bool              `json:"executionDiagnostics,omitempty"`
	BodyArgument         string            `json:"bodyArgument,omitempty"`
	Response             string            `json:"response,omitempty"`
	Name                 string            `json:"name"`
	Description          string            `json:"description"`
	InputSchema          json.RawMessage   `json:"inputSchema,omitempty"`
	OutputSchema         json.RawMessage   `json:"outputSchema,omitempty"`
	ReadOnly             bool              `json:"readOnly"`
	Method               string            `json:"method,omitempty"`
	Path                 string            `json:"path,omitempty"`
	Query                map[string]string `json:"query,omitempty"`
	QueryLists           map[string]string `json:"queryLists,omitempty"`
	Range                *HTTPRange        `json:"range,omitempty"`
	Body                 bool              `json:"body,omitempty"`
	SchemaHash           string            `json:"schemaHash,omitempty"`
}
type Info struct {
	Provider           string `json:"provider,omitempty"`
	ID                 string `json:"id"`
	Label              string `json:"label"`
	Kind               string `json:"kind"`
	ExternalProcessing bool   `json:"externalProcessing"`
	CodeOnly           bool   `json:"codeOnly,omitempty"`
}
type Receipt struct {
	ID           string `json:"id"`
	Connection   string `json:"connection"`
	Operation    string `json:"operation"`
	SchemaHash   string `json:"schemaHash"`
	InputHash    string `json:"inputHash"`
	OutputHash   string `json:"outputHash"`
	Time         string `json:"time"`
	DurationMS   int64  `json:"durationMs"`
	Complete     bool   `json:"complete"`
	Verification string `json:"verification"`
}
type Result struct {
	Data    any     `json:"data"`
	Receipt Receipt `json:"receipt"`
}
type runtime struct {
	config                       Connection
	client                       *http.Client
	slots                        chan struct{}
	redshift                     redshiftClient
	aws                          aws.CredentialsProvider
	loginMu                      sync.Mutex
	loginToken, loginFingerprint string
	loginScopes                  string
	loginUntil                   time.Time
	// identity is Contract's digest of the configuration, and arguments the
	// configured input schemas CheckArguments applies.
	identity  string
	arguments map[string]*jsonschema.Resolved
	// configured is the contract of a connection whose configuration defines
	// it; an MCP or broker connection instead keeps its upstream's last answer.
	configured    *contract
	configuredErr error
	contractMu    sync.Mutex
	discovered    *contract
}
type Manager struct {
	connections map[string]*runtime
	mu          sync.Mutex
	closed      bool
}

var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,95}$`)

const emptySchema = `{"type":"object","properties":{},"additionalProperties":false}`

func Load(file string) (*Manager, error) {
	var cfg Config
	if err := jsonfile.Load(file, 2<<20, &cfg); err != nil {
		return nil, errors.New("invalid data connection configuration")
	}
	return New(cfg)
}

func New(cfg Config) (*Manager, error) {
	if cfg.Version != 1 || len(cfg.Connections) > 128 {
		return nil, errors.New("data connections require version 1 and at most 128 connections")
	}
	m := &Manager{connections: map[string]*runtime{}}
	for _, c := range cfg.Connections {
		if !identifier.MatchString(c.ID) || m.connections[c.ID] != nil {
			return nil, errors.New("invalid or duplicate connection ID")
		}
		if c.Label == "" {
			c.Label = c.ID
		}
		if len(c.Label) > 120 {
			return nil, ErrInvalid
		}
		if c.SecretFile != "" && (!filepath.IsAbs(c.SecretFile) || c.SecretEnv != "") {
			return nil, errors.New("use one absolute secretFile or secretEnv")
		}
		if c.SecretEnv != "" && !regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`).MatchString(c.SecretEnv) {
			return nil, ErrInvalid
		}
		if c.Kind != "http" && c.Kind != "mcp" && c.Kind != "postgres" && c.Kind != "redshift" && c.Kind != "broker" && c.Kind != "email" && c.Kind != "sheets" && c.Kind != "calendar" {
			return nil, errors.New("unknown connection kind")
		}
		if c.Kind == "postgres" || c.Kind == "redshift" {
			if err := integrations.Check(c.Kind); err != nil {
				return nil, fmt.Errorf("connection %s: %w", c.ID, err)
			}
		}
		if err := configureGoogle(&c); err != nil {
			return nil, err
		}
		u, err := url.Parse(c.Endpoint)
		if err != nil || u.Host == "" && !(c.Kind == "redshift" && c.Endpoint == "") || u.Fragment != "" || u.RawQuery != "" && c.Kind != "postgres" {
			return nil, errors.New("invalid connection endpoint")
		}
		if c.Kind == "postgres" {
			if u.Scheme != "postgres" && u.Scheme != "postgresql" {
				return nil, ErrInvalid
			}
			if _, password := u.User.Password(); password {
				return nil, errors.New("database passwords belong in runtime secrets")
			}
			if u.User.Username() == "" || u.Path == "" || u.Path == "/" {
				return nil, errors.New("database endpoint requires explicit user and database")
			}
			for key := range u.Query() {
				if key != "sslmode" && key != "sslrootcert" {
					return nil, errors.New("unsupported database endpoint setting")
				}
			}
			if u.Query().Get("sslmode") != "verify-full" && !(u.Query().Get("sslmode") == "disable" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1")) {
				return nil, errors.New("remote databases require sslmode=verify-full")
			}
		} else if !(c.Kind == "redshift" && c.Endpoint == "") && (u.User != nil || u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1" || c.AllowClusterHTTP && clusterService.MatchString(u.Hostname())))) {
			return nil, errors.New("connection endpoint requires HTTPS (loopback HTTP is allowed)")
		}
		if c.HostHeader != "" && (c.Kind != "http" || len(c.HostHeader) > 253 || !httpHost.MatchString(c.HostHeader)) {
			return nil, errors.New("hostHeader requires an HTTP connection and a DNS hostname without a port")
		}
		if c.TimeoutSeconds == 0 {
			c.TimeoutSeconds = 30
		}
		if c.MaxBytes == 0 {
			c.MaxBytes = 1 << 20
		}
		if c.MaxRows == 0 {
			c.MaxRows = 1000
		}
		if c.TimeoutSeconds < 1 || int64(c.TimeoutSeconds) > math.MaxInt64/int64(time.Second) || c.MaxBytes < 1024 || c.MaxBytes > math.MaxInt/64 || c.MaxRows < 1 || c.Concurrent < 0 {
			return nil, errors.New("invalid connection limits")
		}
		if c.AuthHeader == "" {
			c.AuthHeader = "Authorization"
		}
		if c.AuthScheme == "" {
			c.AuthScheme = "Bearer"
		}
		if !regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]*$`).MatchString(c.AuthHeader) || strings.ContainsAny(c.AuthScheme, "\r\n") || strings.EqualFold(c.AuthHeader, "Host") {
			return nil, ErrInvalid
		}
		seen := map[string]bool{}
		arguments := map[string]*jsonschema.Resolved{}
		// Keep runtime policy independent of both the caller's configuration and
		// the public operation descriptors returned for inspection.
		c.Operations = slices.Clone(c.Operations)
		for i := range c.Operations {
			op := &c.Operations[i]
			*op = cloneOperation(*op)
			if c.Kind == "email" || c.Kind == "sheets" || c.Kind == "calendar" {
				if err := managedOperation(c.Kind, op); err != nil {
					return nil, err
				}
			}
			if c.Kind == "postgres" {
				if err = postgresOperation(op); err != nil {
					return nil, err
				}
			}
			if c.Kind == "redshift" {
				if err = redshiftOperation(op); err != nil {
					return nil, err
				}
			}
			if !identifier.MatchString(op.Name) || seen[op.Name] || len(op.Description) > 4000 {
				return nil, errors.New("invalid or duplicate operation")
			}
			seen[op.Name] = true
			if c.Kind != "mcp" && c.Kind != "broker" && len(op.InputSchema) == 0 {
				op.InputSchema = json.RawMessage(emptySchema)
			}
			if len(op.InputSchema) > 0 {
				if arguments[op.Name], err = resolveSchema(op.InputSchema); err != nil {
					return nil, fmt.Errorf("invalid input schema for %s", op.Name)
				}
			}
			if len(op.OutputSchema) > 0 {
				if _, err = resolveSchema(op.OutputSchema); err != nil {
					return nil, fmt.Errorf("invalid output schema for %s", op.Name)
				}
			}
			if op.ExecutionDiagnostics && c.Kind != "http" {
				return nil, errors.New("execution diagnostics require an HTTP connection")
			}
			if c.Kind == "http" {
				if err := validateHTTPQuery(op); err != nil {
					return nil, err
				}
				if op.Method == "" {
					op.Method = "GET"
				}
				if op.Method != "GET" && op.Method != "POST" && op.Method != "PUT" && op.Method != "PATCH" && op.Method != "DELETE" {
					return nil, ErrInvalid
				}
				if !strings.HasPrefix(op.Path, "/") || strings.HasPrefix(op.Path, "//") || strings.ContainsAny(op.Path, "?#\\") || strings.Contains(op.Path, "..") {
					return nil, errors.New("operation path must remain inside configured endpoint")
				}
			}
		}
		if c.CodeOnly {
			if c.ExternalProcessing {
				return nil, errors.New("code-only connections cannot permit external AI processing")
			}
			for _, op := range c.Operations {
				if !op.ReadOnly {
					return nil, errors.New("code-only connections require read-only operations")
				}
			}
		}
		if len(c.Operations) == 0 || len(c.Operations) > 256 {
			return nil, errors.New("configure explicit operations")
		}
		if c.Redshift != nil {
			owned := *c.Redshift
			owned.Schemas = slices.Clone(owned.Schemas)
			owned.DeniedColumns = slices.Clone(owned.DeniedColumns)
			c.Redshift = &owned
		}
		if c.Broker != nil {
			owned := *c.Broker
			c.Broker = &owned
		}
		if err := validateHTTPAuth(&c); err != nil {
			return nil, err
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		if c.CAFile != "" {
			raw, err := os.ReadFile(c.CAFile)
			if err != nil {
				return nil, errors.New("cannot read connection CA")
			}
			roots, err := x509.SystemCertPool()
			if err != nil {
				roots = x509.NewCertPool()
			}
			if !roots.AppendCertsFromPEM(raw) {
				return nil, errors.New("invalid connection CA")
			}
			transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
		}
		var slots chan struct{}
		if c.Concurrent > 0 {
			slots = make(chan struct{}, c.Concurrent)
		}
		raw, _ := json.Marshal(c)
		identity := sha256.Sum256(raw)
		m.connections[c.ID] = &runtime{config: c, slots: slots, client: noRedirectClient(transport, time.Duration(c.TimeoutSeconds)*time.Second), identity: hex.EncodeToString(identity[:]), arguments: arguments}
		if !discovers(c.Kind) {
			// A pin that no longer matches refuses this connection's calls, not startup.
			m.connections[c.ID].configured, m.connections[c.ID].configuredErr = newContract(c.Operations)
		}
		if c.AWS != nil {
			if err := m.connections[c.ID].configureHTTPSigning(); err != nil {
				m.Close()
				return nil, err
			}
		}
		if c.Kind == "redshift" {
			if err := m.connections[c.ID].configureRedshift(); err != nil {
				m.Close()
				return nil, err
			}
		} else if c.Redshift != nil {
			return nil, ErrInvalid
		}
		if c.Kind == "broker" {
			if err := validateBroker(c); err != nil {
				return nil, err
			}
		} else if c.Broker != nil {
			return nil, ErrInvalid
		}
	}
	return m, nil
}

func resolveSchema(raw json.RawMessage) (*jsonschema.Resolved, error) {
	var schema jsonschema.Schema
	if json.Unmarshal(raw, &schema) != nil {
		return nil, ErrInvalid
	}
	return schema.Resolve(nil)
}
func hash(v any) string {
	raw, _ := json.Marshal(v)
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}
func (m *Manager) get(id string) (*runtime, error) {
	if m == nil {
		return nil, ErrUnavailable
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.connections[id]
	if r == nil || m.closed {
		return nil, ErrUnavailable
	}
	return r, nil
}
func (m *Manager) List() []Info {
	out := []Info{}
	if m == nil {
		return out
	}
	for id := range m.connections {
		if info, ok := m.Info(id); ok {
			out = append(out, info)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
func (m *Manager) Info(id string) (Info, bool) {
	r, err := m.get(id)
	if err != nil {
		return Info{}, false
	}
	c := r.config
	info := Info{ID: c.ID, Label: c.Label, Kind: c.Kind, ExternalProcessing: c.ExternalProcessing, CodeOnly: c.CodeOnly}
	if c.Email != nil {
		info.Provider = c.Email.Provider
	}
	return info, true
}

// ConfiguredOperation exposes operator policy without contacting an upstream.
// Call validates against the remote schema before execution, as discovered
// within discoveryLifetime or again when the caller's pin differs from it.
// CheckArguments validates input against a configured schema without calling
// the operation. MCP and broker schemas are only known to a live session, so
// their calls are validated when they run.
func (m *Manager) CheckArguments(id, name string, args map[string]any) error {
	r, err := m.get(id)
	if err != nil {
		return err
	}
	for _, op := range r.config.Operations {
		if op.Name != name {
			continue
		}
		if len(op.InputSchema) == 0 {
			return nil
		}
		raw, err := json.Marshal(args)
		if err != nil || len(raw) > r.config.MaxBytes {
			return errors.New("arguments exceed the connection's configured maxBytes")
		}
		schema := r.arguments[name]
		if schema == nil || args == nil || schema.Validate(map[string]any(args)) != nil {
			return errors.New("arguments do not match operation schema")
		}
		return nil
	}
	return ErrInvalid
}
func (m *Manager) ConfiguredOperation(id, name string) (Operation, bool) {
	r, err := m.get(id)
	if err == nil {
		for _, op := range r.config.Operations {
			if op.Name == name {
				return publicOperation(op), true
			}
		}
	}
	return Operation{}, false
}

// HTTPContract pins the configured destination, credential references, schema
// and request mapping without exposing them to models or public discovery.
func (m *Manager) Contract(id string) (string, bool) {
	r, err := m.get(id)
	if err != nil {
		return "", false
	}
	return r.identity, true
}
func (r *runtime) secret() (string, error) {
	return readCredential(r.config.SecretFile, r.config.SecretEnv)
}
func readCredential(file, env string) (string, error) {
	if file != "" {
		value, err := durable.ReadSecret(file, 64<<10)
		switch {
		case errors.Is(err, durable.ErrNotRegular), errors.Is(err, durable.ErrTooLarge):
			return "", errors.New("invalid connection credential file")
		case errors.Is(err, durable.ErrRead):
			return "", ErrUnavailable
		case err != nil:
			return "", errors.New("connection credential unavailable")
		}
		return value, nil
	}
	value := ""
	if env != "" {
		value = strings.TrimSpace(os.Getenv(env))
	}
	if strings.ContainsAny(value, "\r\n") || value == "" && env != "" {
		return "", errors.New("connection credential unavailable")
	}
	return value, nil
}
func cloneOperation(op Operation) Operation {
	op.InputSchema = bytes.Clone(op.InputSchema)
	op.OutputSchema = bytes.Clone(op.OutputSchema)
	op.Query = maps.Clone(op.Query)
	op.QueryLists = maps.Clone(op.QueryLists)
	if op.Range != nil {
		owned := *op.Range
		op.Range = &owned
	}
	return op
}
func publicOperation(op Operation) Operation {
	op = cloneOperation(op)
	op.Method = ""
	op.Path = ""
	op.Query = nil
	op.QueryLists = nil
	op.Range = nil
	op.Body = false
	return op
}

// Every adapter exposes and checks the same contract identity. A configured pin
// is a constraint on that identity, never an input to its own hash.
func checkedOperation(op Operation) (Operation, error) {
	pin := op.SchemaHash
	op = publicOperation(op)
	if len(op.Description) > 4000 {
		op.Description = op.Description[:4000]
	}
	op.SchemaHash = ""
	op.SchemaHash = hash(op)
	if pin != "" && pin != op.SchemaHash {
		return Operation{}, errors.New("configured operation schema changed")
	}
	return op, nil
}
func (m *Manager) Tools(ctx context.Context, id string) ([]Operation, error) {
	r, err := m.get(id)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(r.config.TimeoutSeconds)*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.slots != nil {
		select {
		case r.slots <- struct{}{}:
			defer func() { <-r.slots }()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err := checkAuthority(ctx); err != nil {
		return nil, err
	}
	return r.tools(ctx)
}
func (r *runtime) tools(ctx context.Context) ([]Operation, error) {
	u := &upstream{runtime: r}
	defer u.close()
	if discovers(r.config.Kind) {
		secret, err := r.secret()
		if err != nil {
			return nil, err
		}
		u.secret = secret
	}
	c, _, err := u.contract(ctx, true)
	if err != nil {
		return nil, err
	}
	return c.public(), nil
}
func (m *Manager) Call(ctx context.Context, id, name string, args json.RawMessage, expected string) (result Result, callErr error) {
	reachedAdapter := false
	defer func() {
		if callErr != nil && !reachedAdapter && m.ManagedWrites(id) {
			callErr = Rejected(callErr.Error())
		}
	}()
	r, err := m.get(id)
	if err != nil {
		return Result{}, err
	}
	if _, ok := m.ConfiguredOperation(id, name); !ok {
		return Result{}, ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(r.config.TimeoutSeconds)*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if r.slots != nil {
		select {
		case r.slots <- struct{}{}:
			defer func() { <-r.slots }()
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}
	if err := checkAuthority(ctx); err != nil {
		return Result{}, err
	}
	start := time.Now()
	secret, err := r.secret()
	if err != nil {
		return Result{}, err
	}
	u := &upstream{runtime: r, secret: secret}
	defer u.close()
	c, cached, err := u.contract(ctx, false)
	if err != nil {
		return Result{}, err
	}
	selected := c.find(name)
	if cached && (selected == nil || expected != "" && selected.SchemaHash != expected) {
		// The caller or the upstream may have moved on since that discovery.
		if c, _, err = u.contract(ctx, true); err != nil {
			return Result{}, err
		}
		selected = c.find(name)
	}
	if selected == nil {
		return Result{}, ErrInvalid
	}
	if expected != "" && selected.SchemaHash != expected {
		return Result{}, ErrSchemaChanged
	}
	if r.managedWrites() && !selected.ReadOnly && ctx.Value(submissionKey{}) != true {
		return Result{}, Rejected("Email and Sheets writes require the durable submission owner")
	}
	var input map[string]any
	if len(args) > r.config.MaxBytes {
		return Result{}, errors.New("arguments exceed the connection's configured maxBytes")
	}
	if json.Unmarshal(args, &input) != nil || input == nil {
		return Result{}, ErrInvalid
	}
	if schema := c.input[name]; schema == nil || schema.Validate(input) != nil {
		return Result{}, errors.New("arguments do not match operation schema")
	}
	if err := checkAuthority(ctx); err != nil {
		return Result{}, err
	}
	var data any
	sensitive := []string{secret}
	complete := true
	switch r.config.Kind {
	case "email", "sheets", "calendar":
		reachedAdapter = true
		var credentials []string
		data, credentials, err = r.googleCall(ctx, name, input)
		sensitive = append(sensitive, credentials...)
	case "http":
		var credentials []string
		data, credentials, err = r.httpCall(ctx, name, input, secret)
		sensitive = append(sensitive, credentials...)
	case "mcp":
		var session *mcp.ClientSession
		if session, err = u.mcpSession(ctx); err == nil {
			data, err = mcpCall(ctx, session, name, input)
		}
	case "postgres":
		data, complete, err = r.postgresCall(ctx, name, input, secret)
	case "redshift":
		data, complete, err = r.redshiftCall(ctx, name, input)
	case "broker":
		data, err = r.brokerCall(ctx, name, input, secret)
	}
	if err != nil {
		r.forgetContract()
		return Result{}, err
	}
	raw, err := json.Marshal(data)
	if err != nil || len(raw) > r.config.MaxBytes {
		return Result{}, errors.New("result exceeds connection limit; narrow the request or request a page")
	}
	// Provider errors and any reflection of the injected credential never escape.
	slices.SortFunc(sensitive, func(a, b string) int { return len(b) - len(a) })
	for _, secret := range sensitive {
		if secret == "" {
			continue
		}
		escaped, _ := json.Marshal(secret)
		raw = []byte(strings.ReplaceAll(string(raw), string(escaped[1:len(escaped)-1]), "[redacted]"))
	}
	if json.Unmarshal(raw, &data) != nil {
		return Result{}, ErrUnavailable
	}
	if len(selected.OutputSchema) > 0 {
		if schema := c.output[name]; schema == nil || schema.Validate(data) != nil {
			r.forgetContract()
			return Result{}, errors.New("result does not match operation schema")
		}
	}
	receipt := Receipt{Connection: id, Operation: name, SchemaHash: selected.SchemaHash, InputHash: hash(input), OutputHash: hash(data), Time: start.UTC().Format(time.RFC3339Nano), DurationMS: time.Since(start).Milliseconds(), Complete: complete, Verification: "executed"}
	receipt.ID = hash(receipt)
	return Result{data, receipt}, nil
}
func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	for _, r := range m.connections {
		r.client.CloseIdleConnections()
	}
}

// Learning is opt-in private retention. Never capture a reflection of this
// connection's injected credential, even when it occurs in an ordinary argument.
// Without data connections nothing is safe to capture.
func (m *Manager) LearningInputSafe(id, raw string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	r, ok := m.connections[id]
	m.mu.Unlock()
	if !ok {
		return false
	}
	if r.managedWrites() {
		return false
	}
	secret, err := r.secret()
	if err != nil {
		return false
	}
	secrets, err := r.httpSecrets()
	if err != nil {
		return false
	}
	for _, value := range append(secrets, secret) {
		if value == "" {
			continue
		}
		escaped, _ := json.Marshal(value)
		if strings.Contains(raw, value) || strings.Contains(raw, string(escaped[1:len(escaped)-1])) {
			return false
		}
	}
	return true
}

// RuntimeFiles is used only by startup validation, never returned to clients.
func (m *Manager) RuntimeFiles() []string {
	out := []string{}
	for _, r := range m.connections {
		if r.config.SecretFile != "" {
			out = append(out, r.config.SecretFile)
		}
		if r.config.UsernameFile != "" {
			out = append(out, r.config.UsernameFile)
		}
		if r.config.Login != nil && r.config.Login.UsernameFile != "" {
			out = append(out, r.config.Login.UsernameFile)
		}
		if c := r.config.GitHubApp; c != nil {
			out = append(out, c.PrivateKeyFile)
		}
		if c := r.config.OAuthClientCredentials; c != nil {
			for _, p := range []string{c.ClientIDFile, c.ClientSecretFile} {
				if p != "" {
					out = append(out, p)
				}
			}
		}
		if r.config.OAuthRefresh != nil {
			if p := r.config.OAuthRefresh.ClientSecretFile; p != "" {
				out = append(out, p)
			}
			if p := r.config.OAuthRefresh.RefreshTokenFile; p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}
