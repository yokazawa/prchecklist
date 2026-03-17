package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	"github.com/google/go-github/v31/github"
	"github.com/stretchr/testify/assert"
	"golang.org/x/oauth2"

	"github.com/motemen/prchecklist/v2"
)

func TestGitHub_GetPullRequest(t *testing.T) {
	token := os.Getenv("PRCHECKLIST_TEST_GITHUB_TOKEN")
	if token == "" {
		t.Skipf("PRCHECKLIST_TEST_GITHUB_TOKEN not set")
	}

	github, err := NewGitHub()
	assert.NoError(t, err)

	ctx := context.Background()
	cli := oauth2.NewClient(
		ctx,
		oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token}),
	)
	ctx = context.WithValue(ctx, prchecklist.ContextKeyHTTPClient, cli)
	_, _, err = github.GetPullRequest(ctx, prchecklist.ChecklistRef{
		Owner:  "motemen",
		Repo:   "test-repository",
		Number: 2,
	}, true)
	assert.NoError(t, err)
}

func TestGitHub_GetPullRequest_MoreThan250Commits(t *testing.T) {
	token := os.Getenv("PRCHECKLIST_TEST_GITHUB_TOKEN")
	if token == "" {
		t.Skipf("PRCHECKLIST_TEST_GITHUB_TOKEN not set")
	}

	gw, err := NewGitHub()
	assert.NoError(t, err)

	ctx := context.Background()
	cli := oauth2.NewClient(
		ctx,
		oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token}),
	)
	ctx = context.WithValue(ctx, prchecklist.ContextKeyHTTPClient, cli)

	// 250 commits
	//   https://github.com/phpbb/phpbb/pull/992
	//   https://github.com/CesiumGS/cesium/pull/286
	//   https://github.com/cappuccino/cappuccino/pull/2068
	pullReq, _, err := gw.GetPullRequest(ctx, prchecklist.ChecklistRef{
		Owner:  "phpbb",
		Repo:   "phpbb",
		Number: 992,
	}, true)
	assert.NoError(t, err)
	assert.NotNil(t, pullReq)

	// Verify that we got more than 250 commits (pagination working)
	assert.Greater(t, len(pullReq.Commits), 250, "Expected more than 250 commits to verify pagination is working")

	// Use REST API to verify the exact commit count
	restClient := github.NewClient(cli)
	restPR, _, err := restClient.PullRequests.Get(ctx, "phpbb", "phpbb", 992)
	assert.NoError(t, err)
	assert.NotNil(t, restPR)

	expectedCommits := restPR.GetCommits()
	assert.Equal(t, expectedCommits, len(pullReq.Commits), "GraphQL commit count should match REST API commit count")
}

func TestGitHub_getPullRequest_fallback(t *testing.T) {
	mux := http.NewServeMux()

	// Mock GraphQL
	mux.HandleFunc("/api/graphql", func(w http.ResponseWriter, r *http.Request) {
		res := map[string]interface{}{
			"data": map[string]interface{}{
				"repository": map[string]interface{}{
					"isPrivate": false,
					"pullRequest": map[string]interface{}{
						"url":    "http://example.com/1",
						"title":  "title",
						"number": 1,
						"body":   "body",
						"author": map[string]interface{}{
							"login": "author",
						},
						"assignees": map[string]interface{}{
							"edges": []interface{}{},
						},
						"baseRef": map[string]interface{}{
							"name": "master",
						},
						"headRef": map[string]interface{}{
							"target": map[string]interface{}{
								"tree": map[string]interface{}{
									"entries": []interface{}{},
								},
							},
						},
						"commits": map[string]interface{}{
							"totalCount": 300, // Trigger fallback
							"edges": []interface{}{
								map[string]interface{}{
									"node": map[string]interface{}{
										"commit": map[string]interface{}{
											"message": "graphql commit",
											"oid":     "abc",
										},
									},
								},
							},
							"pageInfo": map[string]interface{}{
								"hasNextPage": false,
								"endCursor":   "",
							},
						},
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(res)
	})

	// Mock REST PR
	mux.HandleFunc("/api/v3/repos/o/r/pulls/1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"commits": 300, "head": {"sha": "headsha"}, "base": {"ref": "master"}}`))
	})

	// Mock REST Compare (return error to exercise CompareCommits failure path)
	mux.HandleFunc("/api/v3/repos/o/r/compare/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	// Mock REST Commits
	mux.HandleFunc("/api/v3/repos/o/r/commits", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`)) // Empty list to trigger fallback
	})

	ts := httptest.NewTLSServer(mux)
	defer ts.Close()

	u, _ := url.Parse(ts.URL)
	g := githubGateway{
		domain: u.Host,
	}

	ctx := context.Background()
	ctx = context.WithValue(ctx, prchecklist.ContextKeyHTTPClient, ts.Client())

	// Capture log output
	var logBuf bytes.Buffer
	originalOutput := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(originalOutput)

	ref := prchecklist.ChecklistRef{Owner: "o", Repo: "r", Number: 1}
	pr, err := g.getPullRequest(ctx, ref, true)

	assert.NoError(t, err)
	assert.NotNil(t, pr)
	assert.Equal(t, 1, len(pr.Commits))
	assert.Equal(t, "graphql commit", pr.Commits[0].Message)

	// Verify log output
	assert.Contains(t, logBuf.String(), "warning: getCommitsByListCommits returned empty commits list, fallback to GraphQL API commits list")
}

// buildCommitJSON builds a JSON array of RepositoryCommit objects for use in test mocks.
func buildCommitJSON(shas []string) string {
type innerCommit struct {
Message string `json:"message"`
}
type commit struct {
SHA    string      `json:"sha"`
Commit innerCommit `json:"commit"`
}
commits := make([]commit, len(shas))
for i, sha := range shas {
commits[i] = commit{SHA: sha, Commit: innerCommit{Message: "msg-" + sha}}
}
b, _ := json.Marshal(commits)
return string(b)
}

// TestGitHub_getCommitsByListCommits_boundaryFound tests that when CompareCommits
// succeeds and the boundary SHA appears in the commit list, only commits before
// the boundary (exclusive) are returned.
func TestGitHub_getCommitsByListCommits_boundaryFound(t *testing.T) {
mux := http.NewServeMux()

mux.HandleFunc("/api/v3/repos/o/r/pulls/1", func(w http.ResponseWriter, r *http.Request) {
w.Header().Set("Content-Type", "application/json")
// targetCount=5; boundary is sha3 so only sha1,sha2 should survive
w.Write([]byte(`{"commits": 5, "head": {"sha": "sha1"}, "base": {"ref": "main"}}`))
})

mux.HandleFunc("/api/v3/repos/o/r/compare/", func(w http.ResponseWriter, r *http.Request) {
w.Header().Set("Content-Type", "application/json")
// merge_base_commit is sha3; sha1,sha2 are "new", sha3+ are "old"
w.Write([]byte(`{"merge_base_commit": {"sha": "sha3", "commit": {"message": "msg-sha3"}}}`))
})

mux.HandleFunc("/api/v3/repos/o/r/commits", func(w http.ResponseWriter, r *http.Request) {
w.Header().Set("Content-Type", "application/json")
// ListCommits returns newest first: sha1, sha2, sha3 (boundary), sha4, sha5
w.Write([]byte(buildCommitJSON([]string{"sha1", "sha2", "sha3", "sha4", "sha5"})))
})

ts := httptest.NewTLSServer(mux)
defer ts.Close()

u, _ := url.Parse(ts.URL)
g := githubGateway{domain: u.Host}

ctx := context.WithValue(context.Background(), prchecklist.ContextKeyHTTPClient, ts.Client())
ref := prchecklist.ChecklistRef{Owner: "o", Repo: "r", Number: 1}

commits, err := g.getCommitsByListCommits(ctx, ref)
assert.NoError(t, err)
// sha3 is the boundary and must not be included; sha4, sha5 are beyond boundary.
// The slice is reversed (oldest-first after reverse), so expected order is sha2, sha1.
assert.Equal(t, []prchecklist.Commit{
{Message: "msg-sha2", Oid: "sha2"},
{Message: "msg-sha1", Oid: "sha1"},
}, commits)
}

// TestGitHub_getCommitsByListCommits_compareCommitsFails tests that when
// CompareCommits returns an error, the function falls back to collecting
// targetCount commits.
func TestGitHub_getCommitsByListCommits_compareCommitsFails(t *testing.T) {
mux := http.NewServeMux()

mux.HandleFunc("/api/v3/repos/o/r/pulls/1", func(w http.ResponseWriter, r *http.Request) {
w.Header().Set("Content-Type", "application/json")
w.Write([]byte(`{"commits": 3, "head": {"sha": "sha1"}, "base": {"ref": "main"}}`))
})

mux.HandleFunc("/api/v3/repos/o/r/compare/", func(w http.ResponseWriter, r *http.Request) {
w.WriteHeader(http.StatusInternalServerError)
})

mux.HandleFunc("/api/v3/repos/o/r/commits", func(w http.ResponseWriter, r *http.Request) {
w.Header().Set("Content-Type", "application/json")
// Return 5 commits; only the first 3 (targetCount) should be taken.
w.Write([]byte(buildCommitJSON([]string{"sha1", "sha2", "sha3", "sha4", "sha5"})))
})

ts := httptest.NewTLSServer(mux)
defer ts.Close()

u, _ := url.Parse(ts.URL)
g := githubGateway{domain: u.Host}

var logBuf bytes.Buffer
log.SetOutput(&logBuf)
defer log.SetOutput(os.Stderr)

ctx := context.WithValue(context.Background(), prchecklist.ContextKeyHTTPClient, ts.Client())
ref := prchecklist.ChecklistRef{Owner: "o", Repo: "r", Number: 1}

commits, err := g.getCommitsByListCommits(ctx, ref)
assert.NoError(t, err)
// targetCount=3, reversed order: sha3, sha2, sha1
assert.Equal(t, []prchecklist.Commit{
{Message: "msg-sha3", Oid: "sha3"},
{Message: "msg-sha2", Oid: "sha2"},
{Message: "msg-sha1", Oid: "sha1"},
}, commits)
assert.Contains(t, logBuf.String(), "warning: Repositories.CompareCommits failed")
}

// TestGitHub_getCommitsByListCommits_boundaryNotFound tests that when
// CompareCommits succeeds but the boundary SHA is not found in the commit list,
// the function falls back to targetCount truncation.
func TestGitHub_getCommitsByListCommits_boundaryNotFound(t *testing.T) {
mux := http.NewServeMux()

mux.HandleFunc("/api/v3/repos/o/r/pulls/1", func(w http.ResponseWriter, r *http.Request) {
w.Header().Set("Content-Type", "application/json")
w.Write([]byte(`{"commits": 2, "head": {"sha": "sha1"}, "base": {"ref": "main"}}`))
})

mux.HandleFunc("/api/v3/repos/o/r/compare/", func(w http.ResponseWriter, r *http.Request) {
w.Header().Set("Content-Type", "application/json")
// boundary SHA is not in the commit list returned by ListCommits
w.Write([]byte(`{"merge_base_commit": {"sha": "notinlist", "commit": {"message": "old"}}}`))
})

mux.HandleFunc("/api/v3/repos/o/r/commits", func(w http.ResponseWriter, r *http.Request) {
w.Header().Set("Content-Type", "application/json")
w.Write([]byte(buildCommitJSON([]string{"sha1", "sha2", "sha3"})))
})

ts := httptest.NewTLSServer(mux)
defer ts.Close()

u, _ := url.Parse(ts.URL)
g := githubGateway{domain: u.Host}

var logBuf bytes.Buffer
log.SetOutput(&logBuf)
defer log.SetOutput(os.Stderr)

ctx := context.WithValue(context.Background(), prchecklist.ContextKeyHTTPClient, ts.Client())
ref := prchecklist.ChecklistRef{Owner: "o", Repo: "r", Number: 1}

commits, err := g.getCommitsByListCommits(ctx, ref)
assert.NoError(t, err)
// boundary not found → fallback to targetCount=2, reversed: sha2, sha1
assert.Equal(t, []prchecklist.Commit{
{Message: "msg-sha2", Oid: "sha2"},
{Message: "msg-sha1", Oid: "sha1"},
}, commits)
assert.Contains(t, logBuf.String(), "warning: boundary SHA notinlist not found in commit list")
}
