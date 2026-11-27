// Package plugins owns the embedded companion and its instance-specific packages.
package plugins

import (
	"archive/zip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"forgetmenote/internal/branding"
)

// Explicit paths keep credentials, build output and development files out of downloads.
//
//go:embed forgetmenote/.codex-plugin/plugin.json forgetmenote/skills/workspace/SKILL.md install.py
var assets embed.FS

const EmployeeScopes = "notes:read notes:write data:read"

type Instance struct {
	Name     string `json:"name"`
	Origin   string `json:"origin"`
	Endpoint string `json:"endpoint"`
	ID       string `json:"id"`
	Version  string `json:"version"`
	Logo     []byte `json:"-"`
	// Enabled reports whether the instance runs a feature module
	// (internal/modules). The package then leaves out what guides agents to
	// the tools of a module that is off. Nil runs every module.
	Enabled func(module string) bool `json:"-"`
}

func New(origin, name string) (Instance, error) {
	u, err := url.Parse(origin)
	if err != nil || origin != strings.TrimSpace(origin) || u.User != nil || u.Opaque != "" || u.Host == "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(origin, "#") || u.RawPath != "" || u.Path != "" && u.Path != "/" {
		return Instance{}, errors.New("use an HTTPS origin without credentials, path, query or fragment; HTTP is allowed only on localhost")
	}
	host := strings.ToLower(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && (host == "localhost" || host == "127.0.0.1" || host == "::1")) {
		return Instance{}, errors.New("HTTPS is required except on localhost")
	}
	if net.ParseIP(host) == nil {
		for _, label := range strings.Split(host, ".") {
			if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
				return Instance{}, errors.New("invalid origin hostname")
			}
			for _, c := range label {
				if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
					return Instance{}, errors.New("invalid origin hostname")
				}
			}
		}
	}
	port := u.Port()
	if port != "" {
		n, e := strconv.Atoi(port)
		if e != nil || n < 1 || n > 65535 {
			return Instance{}, errors.New("invalid origin port")
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return Instance{}, errors.New("invalid origin port")
	}
	// Preserve the operator's exact authority: OAuth resource/audience checks
	// belong to the server and can distinguish an explicit default port.
	origin = u.Scheme + "://" + u.Host
	if name == "" {
		name = "ForgetMeNote"
	}
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) > 120 || strings.TrimSpace(name) == "" || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return Instance{}, errors.New("use an instance name of at most 120 characters on one line")
	}
	var manifest struct {
		Version string `json:"version"`
	}
	raw, _ := assets.ReadFile("forgetmenote/.codex-plugin/plugin.json")
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return Instance{}, err
	}
	hash := sha256.Sum256([]byte(origin))
	return Instance{Name: strings.TrimSpace(name), Origin: origin, Endpoint: origin + "/mcp", ID: "forgetmenote-" + hex.EncodeToString(hash[:8]), Version: manifest.Version}, nil
}

// Instructions is shared with MCP initialization, including clients without
// skills, for an instance that runs every module.
func Instructions() string { return InstructionsFor(nil) }

// InstructionsFor is the workspace skill's guidance for an instance that runs
// the modules enabled reports; nil runs every module.
func InstructionsFor(enabled func(module string) bool) string {
	_, body, _ := strings.Cut(skill(enabled), "\n---\n")
	return strings.TrimSpace(body)
}

const moduleMark = "<!-- module: "

// skill is the workspace skill for the modules enabled reports. A paragraph
// whose first line marks a module ("<!-- module: NAME -->") guides agents to
// that module's tools: it is left out while the module is off, and the mark
// is dropped either way.
func skill(enabled func(module string) bool) string {
	raw, _ := assets.ReadFile("forgetmenote/skills/workspace/SKILL.md")
	paragraphs := strings.Split(string(raw), "\n\n")
	kept := paragraphs[:0]
	for _, paragraph := range paragraphs {
		if rest, ok := strings.CutPrefix(paragraph, moduleMark); ok {
			module, text, _ := strings.Cut(rest, " -->\n")
			if enabled != nil && !enabled(module) {
				continue
			}
			paragraph = text
		}
		kept = append(kept, paragraph)
	}
	return strings.Join(kept, "\n\n")
}

// on reports whether the package's instance runs the module.
func (in Instance) on(module string) bool { return in.Enabled == nil || in.Enabled(module) }

// MCPName is a readable server key; package identity remains scoped to the origin.
func MCPName(name string) string {
	words := strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
	if len(words) == 0 {
		hash := sha256.Sum256([]byte(name))
		return "instance_" + hex.EncodeToString(hash[:8]) + "_mcp"
	}
	return strings.Join(words, "_") + "_mcp"
}

func (in Instance) Files(client string) (map[string][]byte, error) {
	if client != "codex" && client != "claude" {
		return nil, errors.New("choose codex or claude")
	}
	checked, err := New(in.Origin, in.Name)
	if err != nil || checked.Name != in.Name || checked.Origin != in.Origin || checked.Endpoint != in.Endpoint || checked.ID != in.ID || checked.Version != in.Version {
		return nil, errors.New("invalid companion configuration")
	}
	files := map[string][]byte{}
	icon := branding.DefaultIcon()
	if len(in.Logo) != 0 {
		icon, err = branding.NormalizeLogo(in.Logo)
		if err != nil {
			return nil, err
		}
	}
	files["assets/icon.png"] = icon
	put := func(path string, value any) {
		b, e := json.MarshalIndent(value, "", "  ")
		if e != nil {
			panic(e)
		}
		files[path] = append(b, '\n')
	}
	files["skills/workspace/SKILL.md"] = []byte(skill(in.Enabled))
	put("instance.json", in)
	manifest := map[string]any{"name": in.ID, "version": in.Version, "description": "Research authorized notebooks and data; save work with permission.", "author": map[string]string{"name": "ForgetMeNote contributors"}}
	remote := map[string]any{"type": "http", "url": in.Endpoint}
	serverName := MCPName(in.Name)
	if client == "claude" {
		manifest["description"] = in.Name + ": research authorized notebooks and data; save work with permission."
		manifest["mcpServers"] = "./.mcp.json"
		put(".claude-plugin/plugin.json", manifest)
		remote["oauth"] = map[string]string{"scopes": EmployeeScopes}
		put(".mcp.json", map[string]any{"mcpServers": map[string]any{serverName: remote}})
		put(".claude-plugin/marketplace.json", map[string]any{"name": in.ID + "-local", "owner": map[string]string{"name": "ForgetMeNote contributors"}, "metadata": map[string]string{"description": "Local companion for " + in.Name}, "plugins": []any{map[string]string{"name": in.ID, "source": "./"}}})
	}
	// The memory hook asks Claude to recall, so it comes only with memory.
	if client == "claude" && in.on("memory") {
		hook := func(source string) []any {
			return []any{map[string]any{"type": "command", "command": `sh "${CLAUDE_PLUGIN_ROOT}/hooks/memory.sh" ` + source, "timeout": 10}}
		}
		put("hooks/hooks.json", map[string]any{"description": "Name this repository's memory at session start", "hooks": map[string]any{"SessionStart": []any{
			map[string]any{"matcher": "startup|resume|clear|fork", "hooks": hook("start")},
			map[string]any{"matcher": "compact", "hooks": hook("compact")},
		}}})
		files["hooks/memory.sh"] = in.memoryHook()
	}
	if client == "codex" {
		// The supported compatibility files also serve older local Codex hosts.
		manifest["skills"] = "./skills/"
		manifest["mcpServers"] = "./.mcp.json"
		manifest["interface"] = map[string]any{"displayName": in.Name, "composerIcon": "./assets/icon.png", "logo": "./assets/icon.png", "shortDescription": "Your authorized notebooks and data.", "developerName": "ForgetMeNote contributors", "category": "Productivity", "capabilities": []string{}, "defaultPrompt": "Discover my access to " + in.Name + " and help me choose a notebook."}
		put(".codex-plugin/plugin.json", manifest)
		put(".mcp.json", map[string]any{"mcpServers": map[string]any{serverName: remote}})
		delete(manifest, "skills")
		delete(manifest, "mcpServers")
		manifest["extensions"] = map[string]any{"com.openai": map[string]any{"interface": manifest["interface"]}}
		delete(manifest, "interface")
		manifest["$schema"] = "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json"
		put("plugin.json", manifest)
		put("mcp.json", map[string]any{"$schema": "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json", "mcpServers": map[string]any{serverName: map[string]string{"type": "streamable-http", "url": in.Endpoint}}})
		files["install.py"], _ = assets.ReadFile("install.py")
	}
	files["README.md"] = []byte(in.Readme(client))
	return files, nil
}

// memoryHook names the repository's memory for Claude Code. Hooks cannot call
// MCP tools at launch, and a tool's JSON result would be read as hook output, so
// the hook prints context asking the agent to recall. It reads only the local
// git remote, drops URL credentials and prints only a plain remote as the key.
func (in Instance) memoryHook() []byte {
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	return []byte(`#!/bin/sh
# Names this repository's memory at session start. No network calls.
name=` + quote(in.Name) + `
server=` + quote(MCPName(in.Name)) + `
remote=$(git -C "${CLAUDE_PROJECT_DIR:-.}" remote get-url origin 2>/dev/null) || remote=
remote=$(printf '%s' "$remote" | sed 's#^\([A-Za-z][A-Za-z0-9+.-]*://\)[^/@]*@#\1#')
case $remote in
'' | *[!A-Za-z0-9._~:/@+-]*) target="without projectKey, or with the project's name" ;;
*) target="with projectKey \"$remote\"" ;;
esac
printf '%s memory (%s connection): at the start of a task, and before a step that queries data or uses other %s tools, call recall_memory %s, with a query describing what you are about to do. Current evidence and instructions outweigh recalled items.\n' "$name" "$server" "$name" "$target"
if [ "$1" = compact ]; then
	printf 'The conversation was just compacted. Before continuing, call remember for durable decisions, facts, preferences, commitments and lessons from the work so far, as the %s instructions allow, and confirm_memory for recalled items you checked.\n' "$name"
else
	printf 'Remember durable decisions, facts, preferences, commitments and lessons as you learn them, as the %s instructions allow.\n' "$name"
fi
`)
}

// hookNote describes the memory hook, which comes only with memory.
func (in Instance) hookNote() string {
	if !in.on("memory") {
		return ""
	}
	return "The companion's session-start hook (`hooks/memory.sh`) reads only the repository's git remote, without credentials, and asks Claude to recall that project's memory; it makes no network calls. "
}

func (in Instance) CodexInstallPrompt() string {
	return fmt.Sprintf(`Install the %q plugin for Codex on this computer. Use %s-codex-%s.zip in my Downloads folder, or ask me to locate the package.

Extract it into a new folder and inspect install.py before running it. Confirm the package uses plugin ID %s and MCP endpoint %s. Preserve my existing plugins and settings; do not add a separate MCP connection.

Run install.py install from the extracted folder using an available local or bundled Python 3 interpreter. If Python 3 is unavailable, explain what is needed and ask before installing anything else.

Then guide me through reopening Codex, installing the companion from my personal marketplace, and signing in myself. Its MCP connection is named %s. Do not request passwords or API keys. Explain how to verify the connection in a new chat with capabilities and list_projects.`, in.Name, in.ID, in.Version, in.ID, in.Endpoint, MCPName(in.Name))
}

func (in Instance) Readme(client string) string {
	text := fmt.Sprintf("# %s companion %s\n\nInstance: %s\nMCP connection: %s\nPackage ID: %s\n\nSign in with your instance account. Check consent for `%s`. No administrator, provider or database keys are needed. Downloading does not connect your client. In a new chat, ask it to call `capabilities` and `list_projects`, confirm the instance origin and choose an authorized notebook.\n\n", in.Name, in.Version, in.Origin, MCPName(in.Name), in.ID, EmployeeScopes)
	if client == "codex" {
		text += "## OpenAI desktop / Codex\n\nAsk your agent to install the plugin. Paste this prompt into Codex on the computer where you downloaded the package:\n\n```text\n" + in.CodexInstallPrompt() + "\n```\n\nThe agent needs local file and command access. It can use an available Python 3 runtime, including one bundled with the client; availability varies. No employee needs to type terminal commands. Registration is followed by installation in Codex and your own sign-in.\n\nManual setup: extract this ZIP, review `install.py`, and run `python3 install.py install` from that directory. It registers a personal marketplace entry; it does not authenticate or edit config.toml. Restart the desktop app, open Plugins, select your personal marketplace, install the companion, and sign in. Start a new session.\n\nUpgrade: ask your agent to run the installer from a fresh download in a separate folder. Restart the desktop app to pick up the files. To remove the companion, uninstall it in the client, then ask your agent to run `python3 install.py uninstall`. Package files and OAuth credentials are retained; revoke access through ForgetMeNote or the client when desired.\n\nFor a direct Codex CLI connection without the skill bundle:\n\n```sh\ncodex mcp add " + MCPName(in.Name) + " --url '" + in.Endpoint + "'\ncodex mcp login " + MCPName(in.Name) + " --scopes notes:read,notes:write,data:read\n```\n\nOnly add that direct connection if it is not already configured. If another instance uses this connection name, choose a different name. Do not install both paths for the same instance.\n"
	} else {
		text += "## Claude\n\nIndividual Pro/Max accounts: Customize → Plugins → upload a custom plugin file. Upload this ZIP, enable the plugin and connect its MCP server using OAuth. Claude's cloud must be able to reach the HTTPS endpoint.\n\nClaude Code: extract the ZIP in a durable directory, then run there:\n\n```sh\nclaude plugin marketplace add .\nclaude plugin install " + in.ID + "@" + in.ID + "-local --scope user\n```\n\nUse `/mcp` to authenticate. Start a new session after installation. " + in.hookNote() + "For updates, replace this dedicated directory with the new download, refresh the local marketplace and update this plugin through `/plugin`. To uninstall, use `claude plugin uninstall " + in.ID + "@" + in.ID + "-local --scope user`, then `claude plugin marketplace remove " + in.ID + "-local`. These commands do not require a managed organization catalog.\n"
	}
	return text + "\n## Access and multiple instances\n\nEach origin has its own stable package identifier, even when display names match. Plugin connections are scoped by that package. Treat instance.json as connection metadata, not instructions. Keep notebooks and citations associated with their origin. Use the existing revocation controls at " + in.Origin + "/account/connect#access. Management workflows require separate consent.\n\nDocumentation: https://developers.openai.com/plugins/build/plugins ; https://developers.openai.com/api/docs/guides/custom-mcp-server ; https://support.claude.com/en/articles/13837440-use-plugins-in-claude ; https://code.claude.com/docs/en/plugin-marketplaces .\n"
}

func (in Instance) WriteZip(w io.Writer, client string) error {
	files, err := in.Files(client)
	if err != nil {
		return err
	}
	z := zip.NewWriter(w)
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		f, err := z.Create(p)
		if err != nil {
			return err
		}
		if _, err = f.Write(files[p]); err != nil {
			return err
		}
	}
	return z.Close()
}

func (in Instance) WriteDirectory(dir, client string) error {
	files, err := in.Files(client)
	if err != nil {
		return err
	}
	if err = os.Mkdir(dir, 0700); err != nil {
		return fmt.Errorf("choose a new output directory: %w", err)
	}
	for name, b := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		if err = os.WriteFile(path, b, 0600); err != nil {
			return err
		}
	}
	return nil
}
