// Package registry searches the official MCP registry for public servers, and
// turns what it finds into something the gateway can connect to.
package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the official MCP registry.
const DefaultBaseURL = "https://registry.modelcontextprotocol.io"

// Client talks to an MCP registry. BaseURL is a field so a test can point it
// at a stub.
type Client struct {
	BaseURL string
	HTTP    *http.Client
}

// New returns a client for the official registry.
func New() *Client {
	return &Client{BaseURL: DefaultBaseURL}
}

// Server is one entry of the registry: a named server, in a version, with the
// ways to run it.
type Server struct {
	Name        string // the registry name, "io.github.owner/repo"
	Description string
	Version     string
	Homepage    string // the repository's URL, when there is one
	Remotes     []Remote
	Packages    []Package
	UpdatedAt   time.Time
}

// ShortName is the part of the name after the slash, for showing.
func (s Server) ShortName() string {
	if i := strings.LastIndex(s.Name, "/"); i >= 0 {
		return s.Name[i+1:]
	}
	return s.Name
}

// Remote is a hosted server the gateway connects to over HTTP.
type Remote struct {
	Type string // "streamable-http" or "sse"
	URL  string
}

// Package is a server that runs locally as a command.
type Package struct {
	RegistryType string // "npm", "pypi", "oci", …
	Identifier   string // the package name or image
	Version      string
	RuntimeHint  string   // "npx", "uvx", "docker", …
	Arguments    []string // the full argument line, in order
	Env          []EnvVar
}

// EnvVar is an environment variable a package asks for.
type EnvVar struct {
	Name        string
	Description string
	Default     string
	Required    bool
	Secret      bool
}

// Page is one page of search results.
type Page struct {
	Servers    []Server
	NextCursor string // empty when the page is the last
}

// Search finds servers by name and description. An empty query lists the
// registry, newest first. cursor continues an earlier search.
func (c *Client) Search(ctx context.Context, query, cursor string, limit int) (Page, error) {
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	u, err := url.Parse(strings.TrimSuffix(base, "/") + "/v0/servers")
	if err != nil {
		return Page{}, err
	}
	q := u.Query()
	if query = strings.TrimSpace(query); query != "" {
		q.Set("search", query)
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Page{}, err
	}
	req.Header.Set("Accept", "application/json")
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 20 * time.Second}
	}
	res, err := httpClient.Do(req)
	if err != nil {
		return Page{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return Page{}, fmt.Errorf("the registry answered %s", res.Status)
	}

	var body struct {
		Servers []struct {
			Server struct {
				Name        string `json:"name"`
				Description string `json:"description"`
				Version     string `json:"version"`
				Repository  struct {
					URL string `json:"url"`
				} `json:"repository"`
				Remotes []struct {
					Type string `json:"type"`
					URL  string `json:"url"`
				} `json:"remotes"`
				Packages []struct {
					RegistryType string `json:"registryType"`
					Identifier   string `json:"identifier"`
					Version      string `json:"version"`
					RuntimeHint  string `json:"runtimeHint"`
					Arguments    []struct {
						Type  string `json:"type"` // "positional" or "named"
						Name  string `json:"name"`
						Value string `json:"value"`
					} `json:"runtimeArguments"`
					PackageArguments []struct {
						Type  string `json:"type"`
						Name  string `json:"name"`
						Value string `json:"value"`
					} `json:"packageArguments"`
					EnvironmentVariables []struct {
						Name        string `json:"name"`
						Description string `json:"description"`
						Default     string `json:"default"`
						Required    bool   `json:"isRequired"`
						Secret      bool   `json:"isSecret"`
					} `json:"environmentVariables"`
				} `json:"packages"`
			} `json:"server"`
			Meta struct {
				Official struct {
					IsLatest  bool      `json:"isLatest"`
					UpdatedAt time.Time `json:"updatedAt"`
				} `json:"io.modelcontextprotocol.registry/official"`
			} `json:"_meta"`
		} `json:"servers"`
		Metadata struct {
			NextCursor string `json:"nextCursor"`
		} `json:"metadata"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return Page{}, fmt.Errorf("the registry's answer could not be read: %w", err)
	}

	page := Page{NextCursor: body.Metadata.NextCursor}
	for _, entry := range body.Servers {
		// Every version of a server is listed; only the latest is of interest.
		if !entry.Meta.Official.IsLatest {
			continue
		}
		s := Server{
			Name:        entry.Server.Name,
			Description: entry.Server.Description,
			Version:     entry.Server.Version,
			Homepage:    entry.Server.Repository.URL,
			UpdatedAt:   entry.Meta.Official.UpdatedAt,
		}
		for _, r := range entry.Server.Remotes {
			s.Remotes = append(s.Remotes, Remote{Type: r.Type, URL: r.URL})
		}
		for _, p := range entry.Server.Packages {
			pkg := Package{
				RegistryType: p.RegistryType,
				Identifier:   p.Identifier,
				Version:      p.Version,
				RuntimeHint:  p.RuntimeHint,
			}
			for _, arg := range p.Arguments {
				pkg.Arguments = append(pkg.Arguments, argument(arg.Type, arg.Name, arg.Value))
			}
			// The identifier sits between the runtime's and the package's
			// arguments, as in "npx -y <identifier> --flag".
			pkg.Arguments = append(pkg.Arguments, p.Identifier)
			for _, arg := range p.PackageArguments {
				pkg.Arguments = append(pkg.Arguments, argument(arg.Type, arg.Name, arg.Value))
			}
			for _, v := range p.EnvironmentVariables {
				pkg.Env = append(pkg.Env, EnvVar{
					Name:        v.Name,
					Description: v.Description,
					Default:     v.Default,
					Required:    v.Required,
					Secret:      v.Secret,
				})
			}
			s.Packages = append(s.Packages, pkg)
		}
		page.Servers = append(page.Servers, s)
	}
	return page, nil
}

// argument renders one of a package's arguments: a positional one is its
// value, a named one its name, with a value glued on when there is one.
func argument(typ, name, value string) string {
	if typ == "named" && name != "" {
		if value != "" {
			return name + "=" + value
		}
		return name
	}
	return value
}

// Install is the way a registry server would be added: a remote URL when the
// registry knows a hosted one, and a local command otherwise.
type Install struct {
	Remote  bool
	URL     string // Remote: the endpoint
	SSE     bool   // Remote: it speaks SSE rather than streamable HTTP
	Command string // Local: the program, "npx" or "uvx" or "docker"
	Args    []string
	Env     []EnvVar
}

// Install chooses how a server would be added, preferring a hosted remote
// over a local command. ok is false when the registry lists neither.
func (s Server) Install() (install Install, ok bool) {
	for _, remote := range s.Remotes {
		if remote.Type == "streamable-http" && remote.URL != "" {
			return Install{Remote: true, URL: remote.URL}, true
		}
	}
	for _, remote := range s.Remotes {
		if remote.Type == "sse" && remote.URL != "" {
			return Install{Remote: true, URL: remote.URL, SSE: true}, true
		}
	}
	for _, pkg := range s.Packages {
		if install, ok := pkg.Install(); ok {
			return install, true
		}
	}
	return Install{}, false
}

// Install turns a package into a local command. ok is false for a kind of
// package the gateway cannot run.
func (p Package) Install() (Install, bool) {
	install := Install{Args: p.Arguments, Env: p.Env}
	switch p.RegistryType {
	case "npm":
		install.Command = p.RuntimeHint
		if install.Command == "" {
			install.Command = "npx"
		}
	case "pypi":
		install.Command = p.RuntimeHint
		if install.Command == "" {
			install.Command = "uvx"
		}
	case "oci":
		// A container runs detached from any terminal the gateway lacks.
		install.Command = "docker"
		install.Args = append([]string{"run", "-i", "--rm"}, p.Arguments...)
	default:
		return Install{}, false
	}
	return install, true
}

// EnvLines is the environment as "KEY=VALUE" lines, one per variable the
// package names: its default when it has one, empty when it is required and
// still to be filled in. Optional variables without a default are left out.
func EnvLines(vars []EnvVar) string {
	var lines []string
	for _, v := range vars {
		if v.Default != "" {
			lines = append(lines, v.Name+"="+v.Default)
		} else if v.Required {
			lines = append(lines, v.Name+"=")
		}
	}
	return strings.Join(lines, "\n")
}
