// Package instanceconfig owns the private operator configuration format and its
// legacy environment migration. Feature owners retain validation and execution.
package instanceconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"

	"forgetmenote/internal/automation"
	"forgetmenote/internal/backups"
	"forgetmenote/internal/blob"
	"forgetmenote/internal/capacity"
	"forgetmenote/internal/chat"
	"forgetmenote/internal/connectors"
	"forgetmenote/internal/decision"
	"forgetmenote/internal/jsonfile"
	"forgetmenote/internal/messaging"
	"forgetmenote/internal/modules"
	"forgetmenote/internal/reposource"
)

const MaximumBytes = 64 << 20

// Matches the interpreter installed by Dockerfile; it is an executable runtime
// default, not another provider/permission source. Explicit sections take priority.
const DockerPython = "/usr/local/bin/python3"

type Server struct {
	Data           string    `json:"data,omitempty"`
	Listen         string    `json:"listen,omitempty"`
	Origin         string    `json:"origin,omitempty"`
	Headless       *bool     `json:"headless,omitempty"`
	BrowseExcluded []string  `json:"browseExcluded,omitempty"`
	Database       *Database `json:"database,omitempty"`
	// Blobs names where the instance keeps files outside Postgres; without
	// it they stay in the data directory.
	Blobs *blob.Config `json:"blobs,omitempty"`
	// Outgoing is "paused" on a restored copy that must not act outside as the
	// instance it was copied from; internal/outgoing validates it.
	Outgoing string `json:"outgoing,omitempty"`
}

// Database names the Postgres control plane. The URL carries a password, so it
// is read from a private file and never stored in this configuration.
type Database struct {
	URLFile string `json:"urlFile"`
	Org     string `json:"org,omitempty"`
}
type SecretClient struct {
	ClientID   string `json:"clientId"`
	SecretFile string `json:"secretFile"`
}
type OIDC struct {
	Issuer        string   `json:"issuer"`
	ClientID      string   `json:"clientId"`
	SecretFile    string   `json:"secretFile"`
	DefaultGroups []string `json:"defaultGroups,omitempty"`
	// RequestEmail adds the email scope to sign-in, for providers such as
	// Google that leave the address out of the ID token without it. The
	// provider's client must allow that scope.
	RequestEmail bool `json:"requestEmail,omitempty"`
}
type Proxy struct {
	SecretFile   string   `json:"secretFile"`
	TrustedCIDRs []string `json:"trustedCidrs"`
}
type Identity struct {
	Admins []string      `json:"admins,omitempty"`
	OIDC   *OIDC         `json:"oidc,omitempty"`
	Proxy  *Proxy        `json:"proxy,omitempty"`
	GitHub *SecretClient `json:"github,omitempty"`
}
type Embeddings struct {
	URL        string `json:"url"`
	SecretFile string `json:"secretFile,omitempty"`
}
type AI struct {
	Chat       *chat.Config     `json:"chat,omitempty"`
	Decisions  *decision.Config `json:"decisions,omitempty"`
	Embeddings *Embeddings      `json:"embeddings,omitempty"`
}
type Integrations struct {
	Data        *connectors.Config         `json:"data,omitempty"`
	Personal    *connectors.PersonalConfig `json:"personal,omitempty"`
	Automations *automation.Config         `json:"automations,omitempty"`
	Python      string                     `json:"python,omitempty"`
	Slack       *messaging.SlackConfig     `json:"slack,omitempty"`
	Repository  *reposource.Config         `json:"repository,omitempty"`
	Backups     *backups.AppConfig         `json:"backups,omitempty"`
}

// Notebooks is the retired section that mapped notebooks to remote notebook
// owners. Every notebook now lives in the instance database, so an owner map
// is refused; the empty section earlier releases wrote is accepted.
type Notebooks struct {
	Owners map[string]json.RawMessage `json:"owners,omitempty"`
}

// errOwnersRemoved explains a configured owner map.
func errOwnersRemoved(setting string) error {
	return fmt.Errorf("%s has been removed: every notebook lives in the instance database, which each gateway serves, so remove the notebook owner map", setting)
}

// Modules turns feature modules off; internal/modules owns their names and
// requirements. Like the server settings, it is read once at startup.
type Modules struct {
	Off []string `json:"off,omitempty"`
}

type Config struct {
	Version      int          `json:"version"`
	Server       Server       `json:"server"`
	Identity     Identity     `json:"identity"`
	AI           AI           `json:"ai"`
	Integrations Integrations `json:"integrations"`
	Modules      Modules      `json:"modules"`
	Notebooks    *Notebooks   `json:"notebooks,omitempty"`
}
type Resolved struct {
	Config Config
	// Private inputs need the same catalog path guard regardless of format.
	Files  []string
	Legacy bool
}

// These are the supported legacy integration controls. New configuration never
// silently merges them with instance.json. Basic container listen/data controls
// remain supported; explicit CLI flags override the sectioned server defaults.
var LegacyEnvironment = []string{
	"CHAT_CONFIG_FILE", "DECISION_CONFIG_FILE", "DATA_CONNECTIONS_CONFIG_FILE", "PERSONAL_CONNECTIONS_CONFIG_FILE", "AUTOMATIONS_CONFIG_FILE", "SLACK_CONFIG_FILE", "REPOSITORY_APP_CONFIG_FILE", "BACKUP_APP_CONFIG_FILE", "AUTOMATION_PYTHON",
	"EMBEDDING_URL", "EMBEDDING_SECRET_FILE", "GITHUB_CLIENT_ID", "GITHUB_CLIENT_SECRET_FILE", "PROXY_SECRET_FILE", "TRUSTED_PROXY_CIDRS", "OIDC_ISSUER_URL", "OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET_FILE", "OIDC_DEFAULT_GROUPS", "OIDC_REQUEST_EMAIL", "ADMIN_USERS", "DATABASE_URL_FILE", "BLOBS_CONFIG_FILE",
	"OUTGOING",
}

func Load(path string) (Config, error) {
	var c Config
	if !filepath.IsAbs(path) {
		return c, errors.New("instance configuration requires an absolute private path")
	}
	if err := jsonfile.Load(path, MaximumBytes, &c); err != nil {
		return c, fmt.Errorf("instance configuration: %w", err)
	}
	if c.Version != 1 {
		return c, errors.New("unsupported instance configuration version")
	}
	if c.Notebooks != nil && len(c.Notebooks.Owners) > 0 {
		return c, errOwnersRemoved("notebooks.owners")
	}
	// A wrong module set stops every command that reads the file, before
	// anything opens; the server installs the same set again at startup.
	if _, err := modules.Parse(c.Modules.Off); err != nil {
		return c, fmt.Errorf("instance configuration: %w", err)
	}
	return c, nil
}

func Resolve(getenv func(string) string) (Resolved, error) {
	if getenv("OWNER_MAP_FILE") != "" {
		return Resolved{}, errOwnersRemoved("OWNER_MAP_FILE")
	}
	if path := getenv("INSTANCE_CONFIG_FILE"); path != "" {
		for _, name := range LegacyEnvironment {
			if name == "AUTOMATION_PYTHON" && getenv(name) == DockerPython {
				continue
			}
			if getenv(name) != "" {
				return Resolved{}, fmt.Errorf("%s conflicts with INSTANCE_CONFIG_FILE; migrate it into instance.json", name)
			}
		}
		c, err := Load(path)
		if c.Integrations.Python == "" && getenv("AUTOMATION_PYTHON") == DockerPython {
			c.Integrations.Python = DockerPython
		}
		return Resolved{Config: c, Files: []string{path}}, err
	}
	return FromLegacy(getenv)
}

func list(raw string) []string {
	var out []string
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}
func FromLegacy(getenv func(string) string) (Resolved, error) {
	r := Resolved{Config: Config{Version: 1}, Legacy: true}
	c := &r.Config
	c.Server = Server{Data: getenv("DATA_DIR"), Listen: getenv("LISTEN_ADDR"), Origin: getenv("PUBLIC_URL"), BrowseExcluded: list(getenv("BROWSE_EXCLUDE_PREFIXES")), Outgoing: getenv("OUTGOING")}
	if raw := getenv("HEADLESS"); raw != "" {
		value := raw == "true"
		c.Server.Headless = &value
	}
	if file := getenv("DATABASE_URL_FILE"); file != "" {
		c.Server.Database = &Database{URLFile: file}
	}
	c.Identity.Admins = list(getenv("ADMIN_USERS"))
	for _, item := range []struct {
		name    string
		maximum int64
		target  any
	}{
		{"CHAT_CONFIG_FILE", 262144, &c.AI.Chat}, {"DECISION_CONFIG_FILE", 65536, &c.AI.Decisions},
		{"DATA_CONNECTIONS_CONFIG_FILE", 2 << 20, &c.Integrations.Data}, {"PERSONAL_CONNECTIONS_CONFIG_FILE", 1 << 20, &c.Integrations.Personal},
		{"AUTOMATIONS_CONFIG_FILE", int64(capacity.StateBytes.Limit()), &c.Integrations.Automations}, {"SLACK_CONFIG_FILE", 256 << 10, &c.Integrations.Slack},
		{"REPOSITORY_APP_CONFIG_FILE", 65536, &c.Integrations.Repository}, {"BACKUP_APP_CONFIG_FILE", 65536, &c.Integrations.Backups}, {"BLOBS_CONFIG_FILE", 65536, &c.Server.Blobs},
	} {
		if path := getenv(item.name); path != "" {
			if err := jsonfile.Load(path, item.maximum, item.target); err != nil {
				return r, fmt.Errorf("%s: %w", item.name, err)
			}
			value := reflect.ValueOf(item.target).Elem()
			if value.Kind() == reflect.Pointer && value.IsNil() {
				return r, fmt.Errorf("%s requires a configuration object", item.name)
			}
			r.Files = append(r.Files, path)
		}
	}
	c.Integrations.Python = getenv("AUTOMATION_PYTHON")
	if address, file := getenv("EMBEDDING_URL"), getenv("EMBEDDING_SECRET_FILE"); address != "" || file != "" {
		c.AI.Embeddings = &Embeddings{URL: address, SecretFile: file}
	}
	if id, file := getenv("GITHUB_CLIENT_ID"), getenv("GITHUB_CLIENT_SECRET_FILE"); id != "" || file != "" {
		c.Identity.GitHub = &SecretClient{ClientID: id, SecretFile: file}
	}
	if file, cidrs := getenv("PROXY_SECRET_FILE"), getenv("TRUSTED_PROXY_CIDRS"); file != "" || cidrs != "" {
		c.Identity.Proxy = &Proxy{SecretFile: file, TrustedCIDRs: list(cidrs)}
	}
	if issuer, id, file := getenv("OIDC_ISSUER_URL"), getenv("OIDC_CLIENT_ID"), getenv("OIDC_CLIENT_SECRET_FILE"); issuer != "" || id != "" || file != "" {
		c.Identity.OIDC = &OIDC{Issuer: issuer, ClientID: id, SecretFile: file, DefaultGroups: list(getenv("OIDC_DEFAULT_GROUPS")), RequestEmail: getenv("OIDC_REQUEST_EMAIL") == "true"}
	}
	return r, nil
}
