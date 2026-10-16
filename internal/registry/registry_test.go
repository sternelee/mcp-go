package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// samplePage is a trimmed-down registry answer: an installable npm server in
// two versions, and a hosted one.
const samplePage = `{
  "servers": [
    {"server": {
      "name": "io.github.example/filesystem",
      "description": "Read and write files.",
      "version": "0.1.2",
      "repository": {"url": "https://github.com/example/fs"},
      "packages": [{
        "registryType": "npm",
        "identifier": "@example/fs-mcp",
        "version": "0.1.2",
        "runtimeHint": "npx",
        "transport": {"type": "stdio"},
        "runtimeArguments": [{"value": "-y", "type": "positional"}],
        "environmentVariables": [
          {"name": "ROOT", "isRequired": true},
          {"name": "DEBUG", "default": "false"}
        ]
      }]
    }, "_meta": {"io.modelcontextprotocol.registry/official": {"isLatest": false}}},
    {"server": {
      "name": "io.github.example/filesystem",
      "description": "Read and write files.",
      "version": "0.1.5",
      "repository": {"url": "https://github.com/example/fs"},
      "packages": [{
        "registryType": "npm",
        "identifier": "@example/fs-mcp",
        "version": "0.1.5",
        "runtimeHint": "npx",
        "transport": {"type": "stdio"},
        "runtimeArguments": [{"value": "-y", "type": "positional"}],
        "environmentVariables": [
          {"name": "ROOT", "isRequired": true},
          {"name": "DEBUG", "default": "false"}
        ]
      }]
    }, "_meta": {"io.modelcontextprotocol.registry/official": {"isLatest": true}}},
    {"server": {
      "name": "io.github.example/hosted",
      "description": "A hosted server.",
      "version": "1.0.0",
      "remotes": [{"type": "streamable-http", "url": "https://mcp.example.com/mcp"}]
    }, "_meta": {"io.modelcontextprotocol.registry/official": {"isLatest": true}}}
  ],
  "metadata": {"nextCursor": "io.github.example/hosted:1.0.0", "count": 3}
}`

func TestSearchParsesAPage(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/servers" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if r.URL.Query().Get("search") != "files" {
			t.Errorf("the query was not passed on: %q", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(samplePage))
	}))
	defer ts.Close()

	c := &Client{BaseURL: ts.URL}
	page, err := c.Search(context.Background(), "files", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	// The older of the two filesystem versions is dropped.
	if len(page.Servers) != 2 {
		t.Fatalf("got %d servers, want the 2 latest ones", len(page.Servers))
	}
	if page.NextCursor != "io.github.example/hosted:1.0.0" {
		t.Errorf("next cursor is %q", page.NextCursor)
	}

	fs := page.Servers[0]
	if fs.Name != "io.github.example/filesystem" || fs.ShortName() != "filesystem" {
		t.Errorf("name %q, short %q", fs.Name, fs.ShortName())
	}
	if fs.Version != "0.1.5" || fs.Homepage != "https://github.com/example/fs" {
		t.Errorf("version %q, homepage %q", fs.Version, fs.Homepage)
	}

	install, ok := fs.Install()
	if !ok || install.Remote {
		t.Fatalf("filesystem should install locally: %+v, %v", install, ok)
	}
	if install.Command != "npx" {
		t.Errorf("command %q, want npx", install.Command)
	}
	wantArgs := []string{"-y", "@example/fs-mcp"}
	if len(install.Args) != len(wantArgs) {
		t.Fatalf("args %v, want %v", install.Args, wantArgs)
	}
	for i, arg := range wantArgs {
		if install.Args[i] != arg {
			t.Errorf("arg %d is %q, want %q", i, install.Args[i], arg)
		}
	}
	if got := EnvLines(install.Env); got != "ROOT=\nDEBUG=false" {
		t.Errorf("env lines %q", got)
	}

	hosted := page.Servers[1]
	install, ok = hosted.Install()
	if !ok || !install.Remote || install.SSE {
		t.Fatalf("hosted should install as a streamable remote: %+v, %v", install, ok)
	}
	if install.URL != "https://mcp.example.com/mcp" {
		t.Errorf("remote URL %q", install.URL)
	}
}

func TestSearchReportsAnHTTPError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer ts.Close()

	c := &Client{BaseURL: ts.URL}
	if _, err := c.Search(context.Background(), "x", "", 10); err == nil {
		t.Fatal("a 502 should be an error")
	}
}

func TestPackageInstallChoosesTheRuntime(t *testing.T) {
	// A Python package runs through uvx when the registry gives no hint.
	pkg := Package{RegistryType: "pypi", Identifier: "mcp-server-x", Arguments: []string{"mcp-server-x"}}
	install, ok := pkg.Install()
	if !ok || install.Command != "uvx" {
		t.Errorf("pypi without a hint: %+v, %v", install, ok)
	}

	// A container image runs through docker, with its arguments after run.
	pkg = Package{RegistryType: "oci", Identifier: "example/image:1", Arguments: []string{"example/image:1"}}
	install, ok = pkg.Install()
	if !ok || install.Command != "docker" {
		t.Fatalf("oci: %+v, %v", install, ok)
	}
	if install.Args[0] != "run" || install.Args[len(install.Args)-1] != "example/image:1" {
		t.Errorf("docker args %v", install.Args)
	}

	// A kind the gateway cannot run is not installable.
	pkg = Package{RegistryType: "nuget", Identifier: "x"}
	if _, ok := pkg.Install(); ok {
		t.Error("nuget should not be installable")
	}
}

func TestNamedArgumentsGlueTheirValueOn(t *testing.T) {
	if got := argument("named", "--config", "x.json"); got != "--config=x.json" {
		t.Errorf("named with a value: %q", got)
	}
	if got := argument("named", "--verbose", ""); got != "--verbose" {
		t.Errorf("named without a value: %q", got)
	}
	if got := argument("positional", "", "-y"); got != "-y" {
		t.Errorf("positional: %q", got)
	}
}
