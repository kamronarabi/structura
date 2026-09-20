// Package ideconfig locates and updates the MCP configuration files that AI
// coding tools read.
//
// Everything in this package writes to a file the user owns and did not ask
// us to reformat. That shapes every decision here: the existing file is
// parsed rather than templated over, servers we did not add are copied
// through byte for byte, a backup is written before the original is touched,
// and a file we cannot parse is refused rather than replaced. Re-running an
// install is a no-op.
//
// The failure this is guarding against is not ours to see. A user who loses
// three other MCP servers because we rewrote their config will find out
// hours later, in a different tool, with no idea what did it.
package ideconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// BackupSuffix is appended to a config's path to name its backup.
const BackupSuffix = ".structura.bak"

// ServerName is the key Structura registers itself under.
const ServerName = "structura"

// Scope selects between a config that applies to one repository and one that
// applies to every project the tool opens.
type Scope string

const (
	// ScopeProject writes inside the repository being scanned.
	ScopeProject Scope = "project"
	// ScopeGlobal writes to the tool's user-level configuration.
	ScopeGlobal Scope = "global"
)

// Client describes one tool's MCP configuration.
type Client struct {
	// Name is the value accepted by --client.
	Name string
	// Title is how the tool is referred to in output.
	Title string
	// Key is the top-level object holding servers. VS Code calls it
	// "servers"; everyone else calls it "mcpServers".
	Key string
	// project and global give the config path for each scope, empty when
	// the tool does not offer that scope.
	project func(root string) string
	global  func(home string) string
	// declaresType reports whether entries carry an explicit "type":"stdio".
	declaresType bool
}

// clients is the supported set, in the order --help lists them.
var clients = []Client{
	{
		Name:  "claude-code",
		Title: "Claude Code",
		Key:   "mcpServers",
		project: func(root string) string {
			return filepath.Join(root, ".mcp.json")
		},
		global: func(home string) string {
			return filepath.Join(home, ".claude.json")
		},
	},
	{
		Name:  "cursor",
		Title: "Cursor",
		Key:   "mcpServers",
		project: func(root string) string {
			return filepath.Join(root, ".cursor", "mcp.json")
		},
		global: func(home string) string {
			return filepath.Join(home, ".cursor", "mcp.json")
		},
	},
	{
		Name:  "windsurf",
		Title: "Windsurf",
		Key:   "mcpServers",
		global: func(home string) string {
			return filepath.Join(home, ".codeium", "windsurf", "mcp_config.json")
		},
	},
	{
		Name:         "vscode",
		Title:        "VS Code",
		Key:          "servers",
		declaresType: true,
		project: func(root string) string {
			return filepath.Join(root, ".vscode", "mcp.json")
		},
		global: func(home string) string {
			switch runtime.GOOS {
			case "darwin":
				return filepath.Join(home, "Library", "Application Support", "Code", "User", "mcp.json")
			case "windows":
				return filepath.Join(home, "AppData", "Roaming", "Code", "User", "mcp.json")
			default:
				return filepath.Join(home, ".config", "Code", "User", "mcp.json")
			}
		},
	},
}

// Clients returns the supported clients.
func Clients() []Client { return append([]Client(nil), clients...) }

// ClientNames returns the accepted --client values.
func ClientNames() []string {
	names := make([]string, 0, len(clients))
	for _, c := range clients {
		names = append(names, c.Name)
	}
	return names
}

// Lookup finds a client by the name the user typed.
func Lookup(name string) (Client, error) {
	for _, c := range clients {
		if strings.EqualFold(c.Name, name) {
			return c, nil
		}
	}
	return Client{}, fmt.Errorf("unknown client %q; supported clients are %s",
		name, strings.Join(ClientNames(), ", "))
}

// Scopes reports which scopes a client supports.
func (c Client) Scopes() []Scope {
	var out []Scope
	if c.project != nil {
		out = append(out, ScopeProject)
	}
	if c.global != nil {
		out = append(out, ScopeGlobal)
	}
	return out
}

// ConfigPath returns where this client's config lives for a scope.
func (c Client) ConfigPath(scope Scope, root, home string) (string, error) {
	switch scope {
	case ScopeProject:
		if c.project == nil {
			return "", fmt.Errorf("%s has no per-project MCP config; install it with --global", c.Title)
		}
		return c.project(root), nil
	case ScopeGlobal:
		if c.global == nil {
			return "", fmt.Errorf("%s has no user-level MCP config; install it with --scope project", c.Title)
		}
		if home == "" {
			return "", errors.New("could not determine the home directory")
		}
		return c.global(home), nil
	default:
		return "", fmt.Errorf("unknown scope %q", scope)
	}
}

// Entry is the server block written into a config.
type Entry struct {
	Command string
	Args    []string
}

// Change is a planned edit, or the absence of one.
type Change struct {
	// Client and Path say what would be written where.
	Client Client
	Path   string
	// Before and After are the file's full contents. Before is empty when
	// the file does not exist yet.
	Before []byte
	After  []byte
	// Created reports whether the file would be brought into existence.
	Created bool
	// AlreadyInstalled reports that the config already says exactly this,
	// so applying the change would write identical bytes.
	AlreadyInstalled bool
	// Replaced reports that a structura entry existed and differed.
	Replaced bool
}

// ErrMalformed reports a config that could not be parsed.
//
// This is a refusal, not a failure to recover. A file we cannot parse is a
// file whose contents we cannot preserve, and overwriting it would destroy
// configuration the user cannot get back.
type ErrMalformed struct {
	Path string
	Err  error
}

func (e *ErrMalformed) Error() string {
	return fmt.Sprintf("%s is not valid JSON (%v).\n"+
		"Structura will not overwrite a file it cannot read without destroying what is in it. "+
		"Fix the JSON — a trailing comma or a // comment is the usual cause — and run this again.",
		e.Path, e.Err)
}

func (e *ErrMalformed) Unwrap() error { return e.Err }

// Plan computes the edit needed to register entry in path, without writing
// anything.
func Plan(client Client, path string, entry Entry) (Change, error) {
	change := Change{Client: client, Path: path}

	before, err := os.ReadFile(path) //nolint:gosec // the path is derived from a known client
	switch {
	case errors.Is(err, os.ErrNotExist):
		change.Created = true
		before = nil
	case err != nil:
		return Change{}, fmt.Errorf("reading %s: %w", path, err)
	default:
		change.Before = before
	}

	// Sibling servers are kept as raw bytes, in their original order, so that
	// installing Structura never reformats, reorders, or drops a server
	// someone else configured.
	root := map[string]json.RawMessage{}
	var rootOrder []string
	if len(bytes.TrimSpace(before)) > 0 {
		if err := json.Unmarshal(before, &root); err != nil {
			return Change{}, &ErrMalformed{Path: path, Err: err}
		}
		rootOrder = keyOrder(before)
	}

	servers := map[string]json.RawMessage{}
	var serverOrder []string
	if raw, ok := root[client.Key]; ok && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &servers); err != nil {
			return Change{}, &ErrMalformed{Path: path, Err: fmt.Errorf("%q is not an object: %w", client.Key, err)}
		}
		serverOrder = keyOrder(raw)
	}

	ours, err := marshalEntry(client, entry)
	if err != nil {
		return Change{}, err
	}
	if existing, ok := servers[ServerName]; ok {
		if equalJSON(existing, ours) {
			change.AlreadyInstalled = true
		} else {
			change.Replaced = true
		}
	}
	servers[ServerName] = ours

	serversRaw, err := marshalObject(servers, serverOrder, 1)
	if err != nil {
		return Change{}, err
	}
	root[client.Key] = serversRaw

	after, err := marshalObject(root, rootOrder, 0)
	if err != nil {
		return Change{}, err
	}
	// Files end with a newline; editors and diff tools both assume it.
	change.After = append(bytes.Clone(after), '\n')
	if change.AlreadyInstalled && bytes.Equal(normalize(change.Before), normalize(change.After)) {
		change.After = change.Before
	}
	return change, nil
}

// NoOp reports whether applying this change would leave the file unchanged.
func (c Change) NoOp() bool { return !c.Created && bytes.Equal(c.Before, c.After) }

// Apply writes the change, backing up the original first.
//
// The backup comes before the write and is verified, because the whole point
// of it is to exist at the moment the write goes wrong.
func Apply(change Change) (backup string, err error) {
	if change.NoOp() {
		return "", nil
	}
	if err := os.MkdirAll(filepath.Dir(change.Path), 0o750); err != nil {
		return "", fmt.Errorf("creating %s: %w", filepath.Dir(change.Path), err)
	}
	if len(change.Before) > 0 {
		backup = change.Path + BackupSuffix
		if err := os.WriteFile(backup, change.Before, 0o600); err != nil {
			return "", fmt.Errorf("writing the backup %s: %w", backup, err)
		}
	}
	// Written via a temporary file so that an interrupted write cannot leave
	// the user with a half-config that their editor then refuses to load.
	tmp, err := os.CreateTemp(filepath.Dir(change.Path), ".structura-mcp-*")
	if err != nil {
		return backup, fmt.Errorf("creating a temporary file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err = tmp.Write(change.After); err != nil {
		_ = tmp.Close()
		return backup, fmt.Errorf("writing %s: %w", tmpName, err)
	}
	if err = tmp.Close(); err != nil {
		return backup, fmt.Errorf("closing %s: %w", tmpName, err)
	}
	if err = os.Chmod(tmpName, configPerm(change)); err != nil {
		return backup, fmt.Errorf("setting permissions on %s: %w", tmpName, err)
	}
	if err = os.Rename(tmpName, change.Path); err != nil {
		return backup, fmt.Errorf("replacing %s: %w", change.Path, err)
	}
	return backup, nil
}

// configPerm keeps an existing file's mode and defaults new files to 0600.
func configPerm(change Change) os.FileMode {
	if info, err := os.Stat(change.Path); err == nil {
		return info.Mode().Perm()
	}
	return 0o600
}

// Diff renders a unified-ish diff of the change, for --dry-run.
//
// It is line-based and deliberately simple: the audience is a person
// deciding whether to let us touch their config, and a handful of changed
// lines is the whole answer.
func (c Change) Diff() string {
	if c.NoOp() {
		return ""
	}
	before := splitLines(string(c.Before))
	after := splitLines(string(c.After))

	// The common prefix and suffix are almost always the bulk of the file.
	prefix := 0
	for prefix < len(before) && prefix < len(after) && before[prefix] == after[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(before)-prefix && suffix < len(after)-prefix &&
		before[len(before)-1-suffix] == after[len(after)-1-suffix] {
		suffix++
	}

	var b strings.Builder
	fmt.Fprintf(&b, "--- %s\n+++ %s\n", label(c.Path, c.Created), c.Path)
	const context = 2
	from := max(prefix-context, 0)
	for _, line := range before[from:prefix] {
		fmt.Fprintf(&b, "  %s\n", line)
	}
	for _, line := range before[prefix : len(before)-suffix] {
		fmt.Fprintf(&b, "- %s\n", line)
	}
	for _, line := range after[prefix : len(after)-suffix] {
		fmt.Fprintf(&b, "+ %s\n", line)
	}
	to := min(len(after)-suffix+context, len(after))
	for _, line := range after[len(after)-suffix : to] {
		fmt.Fprintf(&b, "  %s\n", line)
	}
	return b.String()
}

func label(path string, created bool) string {
	if created {
		return path + " (does not exist yet)"
	}
	return path
}

func splitLines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// marshalEntry renders our server block.
func marshalEntry(client Client, entry Entry) (json.RawMessage, error) {
	// An ordered struct rather than a map, so the rendering is stable and
	// reads the way the tools' own documentation writes it.
	type stdioEntry struct {
		Type    string   `json:"type,omitempty"`
		Command string   `json:"command"`
		Args    []string `json:"args,omitempty"`
	}
	e := stdioEntry{Command: entry.Command, Args: entry.Args}
	if client.declaresType {
		e.Type = "stdio"
	}
	// Indented here rather than by the writer, because the writer copies
	// every value through untouched; see marshalObject.
	raw, err := json.MarshalIndent(e, entryIndent, "  ")
	if err != nil {
		return nil, fmt.Errorf("rendering the %s entry: %w", ServerName, err)
	}
	return raw, nil
}

// entryIndent is the leading whitespace of a server entry: root object, then
// the servers object, then the entry itself.
const entryIndent = "    "

// marshalObject renders a map of raw values at the given indentation depth.
//
// Values are written exactly as they were read. Running them back through
// json.Indent would reflow arrays and objects the user wrote by hand, which
// turns "add one server" into a diff across their whole config — and makes it
// far harder for them to see what we actually changed. Nesting depth never
// changes here, so the original indentation still lines up.
//
// Keys keep their original order, with anything new appended, for the same
// reason.
func marshalObject(obj map[string]json.RawMessage, order []string, depth int) (json.RawMessage, error) {
	if len(obj) == 0 {
		return json.RawMessage("{}"), nil
	}
	keys := orderedKeys(obj, order)

	outer := strings.Repeat("  ", depth)
	inner := outer + "  "

	var b bytes.Buffer
	b.WriteString("{\n")
	for i, k := range keys {
		key, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		b.WriteString(inner)
		b.Write(key)
		b.WriteString(": ")
		b.Write(bytes.TrimSpace(obj[k]))
		if i < len(keys)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString(outer)
	b.WriteString("}")
	return json.RawMessage(b.Bytes()), nil
}

// orderedKeys lists obj's keys in their original order, appending any that
// were not there before in a stable order of their own.
func orderedKeys(obj map[string]json.RawMessage, order []string) []string {
	keys := make([]string, 0, len(obj))
	seen := make(map[string]bool, len(obj))
	for _, k := range order {
		if _, ok := obj[k]; ok && !seen[k] {
			keys = append(keys, k)
			seen[k] = true
		}
	}
	var added []string
	for k := range obj {
		if !seen[k] {
			added = append(added, k)
		}
	}
	sort.Strings(added)
	return append(keys, added...)
}

// keyOrder reports the order in which an object's keys appear in the source,
// which json.Unmarshal into a map discards.
func keyOrder(raw []byte) []string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil
	}
	var keys []string
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return keys
		}
		name, ok := key.(string)
		if !ok {
			return keys
		}
		keys = append(keys, name)
		var discard json.RawMessage
		if err := dec.Decode(&discard); err != nil {
			return keys
		}
	}
	return keys
}

// equalJSON compares two values structurally rather than byte for byte, so a
// difference in whitespace does not read as a change to install.
func equalJSON(a, b json.RawMessage) bool {
	var av, bv any
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		return false
	}
	ab, err1 := json.Marshal(av)
	bb, err2 := json.Marshal(bv)
	return err1 == nil && err2 == nil && bytes.Equal(ab, bb)
}

func normalize(b []byte) []byte {
	var v any
	if json.Unmarshal(b, &v) != nil {
		return b
	}
	out, err := json.Marshal(v)
	if err != nil {
		return b
	}
	return out
}
