package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func entityWith(location string, steps ...map[string]interface{}) map[string]interface{} {
	var s []interface{}
	for _, st := range steps {
		s = append(s, st)
	}
	return map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]interface{}{
				"backstage.io/managed-by-location": location,
			},
		},
		"spec": map[string]interface{}{"steps": s},
	}
}

func step(action string, input map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{"action": action, "input": input}
}

func TestTemplateNeedsLocalContent(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]interface{}
		want bool
	}{
		{"relative content", step("fetch:template", map[string]interface{}{"url": "./content"}), true},
		{"parent dir", step("fetch:template", map[string]interface{}{"url": "../shared"}), true},
		{"remote url", step("fetch:template", map[string]interface{}{"url": "https://github.com/o/r/tree/main/x"}), false},
		{"nested", step("x", map[string]interface{}{"files": []interface{}{map[string]interface{}{"url": "./a"}}}), true},
		{"no url", step("publish:github", map[string]interface{}{"repoUrl": "github.com?repo=x"}), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := templateNeedsLocalContent(entityWith("url:x", c.in)); got != c.want {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestParseGitHubLocation(t *testing.T) {
	loc, err := parseGitHubLocation("https://github.com/stuttgart-things/stuttgart-things/blob/main/backstage/templates/create-terraform-vm/template.yaml", true)
	if err != nil {
		t.Fatal(err)
	}
	want := githubLocation{"stuttgart-things", "stuttgart-things", "main", "backstage/templates/create-terraform-vm"}
	if loc != want {
		t.Fatalf("got %+v, want %+v", loc, want)
	}

	loc, err = parseGitHubLocation("https://github.com/o/r/tree/main/templates/x/", false)
	if err != nil || loc.Dir != "templates/x" {
		t.Fatalf("tree: got %+v, %v", loc, err)
	}

	loc, err = parseGitHubLocation("https://github.com/o/r/blob/main/template.yaml", true)
	if err != nil || loc.Dir != "" {
		t.Fatalf("root: got %+v, %v", loc, err)
	}

	for _, bad := range []string{"https://gitlab.com/o/r/blob/main/t.yaml", "https://github.com/o/r"} {
		if _, err := parseGitHubLocation(bad, true); err == nil {
			t.Fatalf("%s: want error", bad)
		}
	}
}

// fakeGitHub serves a trees listing and the blobs in it.
func fakeGitHub(t *testing.T, files map[string]string, truncated bool) *httptest.Server {
	t.Helper()
	type entry struct {
		Path string `json:"path"`
		Type string `json:"type"`
		SHA  string `json:"sha"`
		Size int    `json:"size"`
	}
	var tree []entry
	blobs := map[string]string{}
	i := 0
	for p, c := range files {
		i++
		sha := strings.Repeat("a", 39) + string(rune('0'+i))
		tree = append(tree, entry{p, "blob", sha, len(c)})
		blobs[sha] = c
	}
	tree = append(tree, entry{"backstage/templates/vm", "tree", "t", 0})
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/repos/o/r/git/trees/main"):
			json.NewEncoder(w).Encode(map[string]interface{}{"tree": tree, "truncated": truncated})
		case strings.HasPrefix(r.URL.Path, "/repos/o/r/git/blobs/"):
			sha := strings.TrimPrefix(r.URL.Path, "/repos/o/r/git/blobs/")
			c, ok := blobs[sha]
			if !ok {
				http.NotFound(w, r)
				return
			}
			enc := base64.StdEncoding.EncodeToString([]byte(c))
			// GitHub wraps blob content at 60 columns.
			var wrapped strings.Builder
			for len(enc) > 60 {
				wrapped.WriteString(enc[:60] + "\n")
				enc = enc[60:]
			}
			wrapped.WriteString(enc + "\n")
			json.NewEncoder(w).Encode(map[string]string{"content": wrapped.String(), "encoding": "base64"})
		default:
			http.NotFound(w, r)
		}
	}))
}

// The regression: create-terraform-vm's dry-run sent an empty
// directoryContents and Backstage failed with ENOENT on ./content.
func TestDryRunDirectoryContentsSendsTemplateDir(t *testing.T) {
	long := strings.Repeat("resource \"x\" {}\n", 20)
	srv := fakeGitHub(t, map[string]string{
		"backstage/templates/vm/template.yaml":       "kind: Template\n",
		"backstage/templates/vm/content/main.tf":     long,
		"backstage/templates/vm-other/template.yaml": "not ours\n",
		"README.md": "root\n",
	}, false)
	defer srv.Close()
	old := githubAPI
	githubAPI = srv.URL
	defer func() { githubAPI = old }()

	e := entityWith("url:https://github.com/o/r/blob/main/backstage/templates/vm/template.yaml",
		step("fetch:template", map[string]interface{}{"url": "./content"}))
	files, err := dryRunDirectoryContents(e)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range files {
		b, err := base64.StdEncoding.DecodeString(f.Base64Content)
		if err != nil {
			t.Fatalf("%s: %v", f.Path, err)
		}
		got[f.Path] = string(b)
	}
	if len(got) != 2 || got["template.yaml"] != "kind: Template\n" || got["content/main.tf"] != long {
		t.Fatalf("got %v", got)
	}
}

func TestDryRunDirectoryContentsWithoutLocalStepsSkipsGitHub(t *testing.T) {
	old := githubAPI
	githubAPI = "http://127.0.0.1:1" // any call would fail
	defer func() { githubAPI = old }()

	e := entityWith("url:https://github.com/o/r/blob/main/t/template.yaml",
		step("publish:github", map[string]interface{}{"repoUrl": "github.com?repo=x"}))
	files, err := dryRunDirectoryContents(e)
	if err != nil || files == nil || len(files) != 0 {
		t.Fatalf("got %v, %v; want empty non-nil list", files, err)
	}
}

func TestDryRunDirectoryContentsFailsClearly(t *testing.T) {
	srv := fakeGitHub(t, map[string]string{"backstage/templates/vm/template.yaml": "x"}, true)
	defer srv.Close()
	old := githubAPI
	githubAPI = srv.URL
	defer func() { githubAPI = old }()

	local := step("fetch:template", map[string]interface{}{"url": "./content"})
	for name, loc := range map[string]string{
		"truncated tree": "url:https://github.com/o/r/blob/main/backstage/templates/vm/template.yaml",
		"file location":  "file:/tmp/template.yaml",
		"not github":     "url:https://gitlab.com/o/r/-/blob/main/template.yaml",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := dryRunDirectoryContents(entityWith(loc, local))
			if err == nil || !strings.Contains(err.Error(), "fetch:template url: ./") {
				t.Fatalf("want a clear error, got %v", err)
			}
		})
	}
}
