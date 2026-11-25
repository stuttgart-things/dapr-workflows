package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// DRY-RUN CONTENT
//
// POST /api/scaffolder/v2/dry-run does not read a template's files from its
// catalog location. The caller sends them in the body as directoryContents,
// paths relative to the template.yaml's directory, and Backstage unpacks them
// into /tmp/dry-run-content-<uuid>/content. Sent empty, that directory is
// never created, and the first step that renders a local directory
// (`fetch:template` with `url: ./content`) fails with
// `ENOENT: lstat '/tmp/dry-run-content-<uuid>/content'` -- which is how a
// dryRun of create-terraform-vm died in under a second on 2026-11-25.
//
// The browser's template editor sends the files from the user's disk. The
// worker has no disk with the template on it, so it reads the directory the
// entity says it came from. Only GitHub locations are supported: that is
// where every template in this fleet lives, and the worker already holds a
// GITHUB_TOKEN for the watch and the merge gate.
// ─────────────────────────────────────────────────────────────────────────────

// dryRunFile is one entry of directoryContents, as the dry-run endpoint takes it.
type dryRunFile struct {
	Path          string `json:"path"`
	Base64Content string `json:"base64Content"`
}

// Bounds on what one dry-run pulls in. A template whose location is a repo's
// root would otherwise send the whole repository to Backstage.
const (
	dryRunMaxFiles = 500
	dryRunMaxBytes = 20 << 20
)

// githubLocation is a directory in a GitHub repository at a ref.
type githubLocation struct {
	Owner, Repo, Ref, Dir string
}

// templateNeedsLocalContent reports whether any step of the template reads
// a path relative to the template (`url: ./content`, `url: ../shared`).
// Those are the steps that need directoryContents; a template without them
// dry-runs fine on an empty list, and is not made to depend on GitHub.
func templateNeedsLocalContent(entity map[string]interface{}) bool {
	spec, _ := entity["spec"].(map[string]interface{})
	steps, _ := spec["steps"].([]interface{})
	for _, s := range steps {
		step, _ := s.(map[string]interface{})
		if hasRelativeURL(step["input"]) {
			return true
		}
	}
	return false
}

func hasRelativeURL(v interface{}) bool {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, val := range t {
			if s, ok := val.(string); ok && k == "url" &&
				(strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../") || s == ".") {
				return true
			}
			if hasRelativeURL(val) {
				return true
			}
		}
	case []interface{}:
		for _, val := range t {
			if hasRelativeURL(val) {
				return true
			}
		}
	}
	return false
}

// templateLocation finds the directory the template was registered from.
// backstage.io/managed-by-location names the template.yaml itself and is
// set by the catalog on every entity it ingested from a URL, so it is
// preferred; backstage.io/source-location is the fallback some
// catalog-info files set by hand. Either is "url:<https URL>".
func templateLocation(entity map[string]interface{}) (string, bool, error) {
	md, _ := entity["metadata"].(map[string]interface{})
	ann, _ := md["annotations"].(map[string]interface{})
	for _, key := range []string{"backstage.io/managed-by-location", "backstage.io/source-location"} {
		v, _ := ann[key].(string)
		if v == "" {
			continue
		}
		if !strings.HasPrefix(v, "url:") {
			return "", false, fmt.Errorf("%s is %q: only url: locations can be read", key, v)
		}
		// managed-by-location points at a file, source-location at a directory.
		return strings.TrimPrefix(v, "url:"), key == "backstage.io/managed-by-location", nil
	}
	return "", false, fmt.Errorf("template has neither backstage.io/managed-by-location nor backstage.io/source-location")
}

// parseGitHubLocation turns https://github.com/<o>/<r>/(blob|tree)/<ref>/<path>
// into its parts. isFile drops the last path element (the template.yaml).
// A ref with a slash in it cannot be told apart from the path that follows,
// so the first segment is taken as the ref; that holds for main and every
// other branch templates are registered from here.
func parseGitHubLocation(raw string, isFile bool) (githubLocation, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return githubLocation{}, fmt.Errorf("location %q: %w", raw, err)
	}
	if u.Host != "github.com" {
		return githubLocation{}, fmt.Errorf("location %q: only github.com is supported", raw)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 4 || (parts[2] != "blob" && parts[2] != "tree") {
		return githubLocation{}, fmt.Errorf("location %q: want github.com/<owner>/<repo>/(blob|tree)/<ref>/<path>", raw)
	}
	dir := strings.Join(parts[4:], "/")
	if isFile {
		dir = path.Dir(dir)
	}
	if dir == "." {
		dir = ""
	}
	return githubLocation{Owner: parts[0], Repo: parts[1], Ref: parts[3], Dir: dir}, nil
}

// fetchGitHubDir reads every file under loc.Dir through the git trees and
// blobs API, and returns them with paths relative to loc.Dir. Blob content
// comes back base64-encoded already, which is what the dry-run takes.
func fetchGitHubDir(loc githubLocation, token string) ([]dryRunFile, error) {
	var tree struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
			SHA  string `json:"sha"`
			Size int64  `json:"size"`
		} `json:"tree"`
		Truncated bool `json:"truncated"`
	}
	treeURL := fmt.Sprintf("%s/repos/%s/%s/git/trees/%s?recursive=1",
		githubAPI, loc.Owner, loc.Repo, url.PathEscape(loc.Ref))
	if err := ghGetJSONOptionalAuth(treeURL, token, &tree); err != nil {
		return nil, fmt.Errorf("list %s/%s@%s: %w", loc.Owner, loc.Repo, loc.Ref, err)
	}
	if tree.Truncated {
		// The listing is incomplete; a partial template would render wrong
		// rather than fail, which is worse than not rendering it at all.
		return nil, fmt.Errorf("list %s/%s@%s: GitHub truncated the tree", loc.Owner, loc.Repo, loc.Ref)
	}

	prefix := ""
	if loc.Dir != "" {
		prefix = loc.Dir + "/"
	}
	var files []dryRunFile
	var total int64
	for _, e := range tree.Tree {
		if e.Type != "blob" || !strings.HasPrefix(e.Path, prefix) {
			continue
		}
		if len(files) >= dryRunMaxFiles {
			return nil, fmt.Errorf("%s has more than %d files", loc.Dir, dryRunMaxFiles)
		}
		if total += e.Size; total > dryRunMaxBytes {
			return nil, fmt.Errorf("%s is larger than %d bytes", loc.Dir, dryRunMaxBytes)
		}
		var blob struct {
			Content  string `json:"content"`
			Encoding string `json:"encoding"`
		}
		blobURL := fmt.Sprintf("%s/repos/%s/%s/git/blobs/%s", githubAPI, loc.Owner, loc.Repo, e.SHA)
		if err := ghGetJSONOptionalAuth(blobURL, token, &blob); err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Path, err)
		}
		if blob.Encoding != "base64" {
			return nil, fmt.Errorf("read %s: unexpected encoding %q", e.Path, blob.Encoding)
		}
		files = append(files, dryRunFile{
			Path:          strings.TrimPrefix(e.Path, prefix),
			Base64Content: strings.ReplaceAll(blob.Content, "\n", ""),
		})
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no files under %q in %s/%s@%s", loc.Dir, loc.Owner, loc.Repo, loc.Ref)
	}
	return files, nil
}

// ghGetJSONOptionalAuth is ghGetJSON without insisting on a token: a public
// template repository reads fine anonymously, within GitHub's lower limit.
func ghGetJSONOptionalAuth(u, token string, out any) error {
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var msg struct {
			Message string `json:"message"`
		}
		json.NewDecoder(resp.Body).Decode(&msg)
		return fmt.Errorf("GET %s HTTP %d: %s", strings.TrimPrefix(u, githubAPI), resp.StatusCode, msg.Message)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// dryRunDirectoryContents is what CallScaffolder sends as directoryContents.
// A template with no relative step URLs gets an empty list, as before. One
// that has them gets its directory, or an error that says why it could not
// be read -- in place of the ENOENT Backstage would answer with otherwise.
func dryRunDirectoryContents(entity map[string]interface{}) ([]dryRunFile, error) {
	if !templateNeedsLocalContent(entity) {
		return []dryRunFile{}, nil
	}
	const why = "template renders local content (fetch:template url: ./...), which a dry-run must be sent"
	raw, isFile, err := templateLocation(entity)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", why, err)
	}
	loc, err := parseGitHubLocation(raw, isFile)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", why, err)
	}
	files, err := fetchGitHubDir(loc, os.Getenv("GITHUB_TOKEN"))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", why, err)
	}
	return files, nil
}
