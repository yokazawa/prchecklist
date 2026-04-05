package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestGitHub_GetPullRequest_UsesCompareCommitsWhenTooManyCommits(t *testing.T) {
	mux := http.NewServeMux()

	randomRefName := func() string {
		const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
		seededRand := rand.New(rand.NewSource(time.Now().UnixNano()))
		b := make([]byte, 20)
		for i := range b {
			b[i] = charset[seededRand.Intn(len(charset))]
		}
		return string(b)
	}

	randomBaseRef := randomRefName()

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
							"name": randomBaseRef,
						},
						"headRef": map[string]interface{}{
							"target": map[string]interface{}{
								"tree": map[string]interface{}{
									"entries": []interface{}{},
								},
							},
						},
						"commits": map[string]interface{}{
							"totalCount": 300,
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

	mux.HandleFunc("/api/v3/repos/o/r/pulls/1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"commits": 300, "head": {"sha": "headsha"}, "base": {"ref": "` + randomBaseRef + `"}}`))
	})

	mux.HandleFunc("/api/v3/repos/o/r/commits/"+randomBaseRef, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fmt.Sprintf(`{
			"sha": %q,
			"parents": [
				{ "sha": "ancestor-sha" }
			]
		}`, randomBaseRef)))
	})

	mux.HandleFunc("/api/v3/repos/o/r/compare/"+randomBaseRef+"...headsha", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"commits": [
				{
					"sha": "c1",
					"commit": { "message": "feature commit 1" }
				},
				{
					"sha": "c2",
					"commit": { "message": "feature commit 2" }
				}
			]
		}`))
	})

	ts := httptest.NewTLSServer(mux)
	defer ts.Close()

	u, _ := url.Parse(ts.URL)
	g := githubGateway{
		domain: u.Host,
	}

	ctx := context.Background()
	ctx = context.WithValue(ctx, prchecklist.ContextKeyHTTPClient, ts.Client())

	ref := prchecklist.ChecklistRef{Owner: "o", Repo: "r", Number: 1}
	pr, err := g.getPullRequest(ctx, ref, true)

	require.NoError(t, err)
	require.NotNil(t, pr)
	require.Len(t, pr.Commits, 2)
	assert.Equal(t, "feature commit 1", pr.Commits[0].Message)
	assert.Equal(t, "feature commit 2", pr.Commits[1].Message)
}

func TestGitHub_getPullRequest_ExcludesReleaseMergeCommit(t *testing.T) {
	type compareCommitKind int

	const (
		compareCommitKindFeature compareCommitKind = iota
		compareCommitKindReleaseMerge
	)

	type testCase struct {
		name                           string
		commits                        []compareCommitKind
		expectedMessages               []string
		expectedSharedParentFetchCount *int
	}

	newFeatureCommit := func(sha, message string) map[string]interface{} {
		return map[string]interface{}{
			"sha": sha,
			"commit": map[string]interface{}{
				"message": message,
			},
			"parents": []map[string]interface{}{
				{"sha": sha + "-parent"},
			},
		}
	}

	newReleaseMergeCommit := func(sha, baseRef, defaultBranchRef, sharedParentSHA string) map[string]interface{} {
		return map[string]interface{}{
			"sha": sha,
			"commit": map[string]interface{}{
				"message": fmt.Sprintf("Merge branch %s into %s", baseRef, defaultBranchRef),
			},
			"parents": []map[string]interface{}{
				{"sha": sharedParentSHA},
				{"sha": baseRef},
			},
		}
	}

	buildCompareCommits := func(kinds []compareCommitKind, baseRef, defaultBranchRef string) []map[string]interface{} {
		sharedParentSHA := "shared-parent-sha"
		commits := make([]map[string]interface{}, 0, len(kinds))
		for i, kind := range kinds {
			switch kind {
			case compareCommitKindFeature:
				commits = append(commits, newFeatureCommit(
					fmt.Sprintf("feature-%d", i+1),
					fmt.Sprintf("feature commit %d", i+1),
				))
			case compareCommitKindReleaseMerge:
				commits = append(commits, newReleaseMergeCommit(
					fmt.Sprintf("release-%d", i+1),
					baseRef,
					defaultBranchRef,
					sharedParentSHA,
				))
			}
		}
		return commits
	}

	expectedOne := 1
	testCases := []testCase{
		{
			name: "release branch merge commit is excluded",
			commits: []compareCommitKind{
				compareCommitKindFeature,
				compareCommitKindReleaseMerge,
				compareCommitKindFeature,
			},
			expectedMessages: []string{"feature commit 1", "feature commit 3"},
		},
		{
			name: "previous release merge commit is excluded",
			commits: []compareCommitKind{
				compareCommitKindFeature,
				compareCommitKindReleaseMerge,
				compareCommitKindFeature,
			},
			expectedMessages: []string{"feature commit 1", "feature commit 3"},
		},
		{
			name: "ancestorMemo caches shared parent ancestry",
			commits: []compareCommitKind{
				compareCommitKindFeature,
				compareCommitKindReleaseMerge,
				compareCommitKindReleaseMerge,
			},
			expectedMessages:               []string{"feature commit 1"},
			expectedSharedParentFetchCount: &expectedOne,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()

			randomRefName := func() string {
				const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
				seededRand := rand.New(rand.NewSource(time.Now().UnixNano()))
				b := make([]byte, 20)
				for i := range b {
					b[i] = charset[seededRand.Intn(len(charset))]
				}
				return string(b)
			}

			baseRef := randomRefName()
			defaultBranchRef := randomRefName()
			compareCommits := buildCompareCommits(tc.commits, baseRef, defaultBranchRef)

			sharedParentFetchCount := 0

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
									"name": baseRef,
								},
								"headRef": map[string]interface{}{
									"target": map[string]interface{}{
										"tree": map[string]interface{}{
											"entries": []interface{}{},
										},
									},
								},
								"commits": map[string]interface{}{
									"totalCount": 300,
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
				_ = json.NewEncoder(w).Encode(res)
			})

			mux.HandleFunc("/api/v3/repos/o/r/pulls/1", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(fmt.Sprintf(`{
					"commits": 300,
					"head": {"sha": "headsha"},
					"base": {"ref": %q}
				}`, baseRef)))
			})

			mux.HandleFunc("/api/v3/repos/o/r/compare/"+baseRef+"...headsha", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"commits": compareCommits,
				})
			})

			mux.HandleFunc("/api/v3/repos/o/r/commits/"+baseRef, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(fmt.Sprintf(`{
					"sha": %q,
					"parents": [
						{ "sha": "ancestor-sha" }
					]
				}`, baseRef)))
			})

			mux.HandleFunc("/api/v3/repos/o/r/commits/ancestor-sha", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{
					"sha": "ancestor-sha",
					"parents": []
				}`))
			})

			mux.HandleFunc("/api/v3/repos/o/r/commits/shared-parent-sha", func(w http.ResponseWriter, r *http.Request) {
				sharedParentFetchCount++
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{
					"sha": "shared-parent-sha",
					"parents": [
						{ "sha": "shared-parent-ancestor" }
					]
				}`))
			})

			mux.HandleFunc("/api/v3/repos/o/r/commits/shared-parent-ancestor", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{
					"sha": "shared-parent-ancestor",
					"parents": []
				}`))
			})

			mux.HandleFunc("/api/v3/repos/o/r/commits/other-parent-sha", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{
					"sha": "other-parent-sha",
					"parents": [
						{ "sha": "other-parent-ancestor-sha" }
					]
				}`))
			})

			mux.HandleFunc("/api/v3/repos/o/r/commits/other-parent-ancestor-sha", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{
					"sha": "other-parent-ancestor-sha",
					"parents": []
				}`))
			})

			for _, commit := range compareCommits {
				parents, _ := commit["parents"].([]map[string]interface{})
				for _, parent := range parents {
					sha, _ := parent["sha"].(string)
					if sha == "" || sha == baseRef || sha == "shared-parent-sha" || sha == "other-parent-sha" {
						continue
					}

					parentSHA := sha
					mux.HandleFunc("/api/v3/repos/o/r/commits/"+parentSHA, func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write([]byte(fmt.Sprintf(`{
							"sha": %q,
							"parents": []
						}`, parentSHA)))
					})
				}
			}

			ts := httptest.NewTLSServer(mux)
			defer ts.Close()

			u, _ := url.Parse(ts.URL)
			g := githubGateway{
				domain: u.Host,
			}

			ctx := context.Background()
			ctx = context.WithValue(ctx, prchecklist.ContextKeyHTTPClient, ts.Client())

			ref := prchecklist.ChecklistRef{Owner: "o", Repo: "r", Number: 1}
			pr, err := g.getPullRequest(ctx, ref, true)

			require.NoError(t, err)
			require.NotNil(t, pr)
			require.Len(t, pr.Commits, len(tc.expectedMessages))

			for i, msg := range tc.expectedMessages {
				assert.Equal(t, msg, pr.Commits[i].Message)
			}

			if tc.expectedSharedParentFetchCount != nil {
				assert.Equal(t, *tc.expectedSharedParentFetchCount, sharedParentFetchCount)
			}
		})
	}
}

func TestGitHub_getPullRequest_ExcludesPreviousReleaseCommitsByHeadSHA(t *testing.T) {
	type commitSpec struct {
		sha     string
		message string
	}

	type testCase struct {
		name             string
		listCommits      []commitSpec
		excludeFromSHA   string
		expectedMessages []string
	}

	testCases := []testCase{
		{
			name: "stops before previous release head and reverses returned commits",
			listCommits: []commitSpec{
				{sha: "new-1", message: "new commit 1"},
				{sha: "new-2", message: "new commit 2"},
				{sha: "prev-release-head", message: "previous release head"},
				{sha: "old-1", message: "old commit 1"},
			},
			excludeFromSHA:   "prev-release-head",
			expectedMessages: []string{"new commit 2", "new commit 1"},
		},
		{
			name: "returns all commits when boundary sha is not present",
			listCommits: []commitSpec{
				{sha: "new-1", message: "new commit 1"},
				{sha: "new-2", message: "new commit 2"},
				{sha: "new-3", message: "new commit 3"},
			},
			excludeFromSHA:   "prev-release-head",
			expectedMessages: []string{"new commit 3", "new commit 2", "new commit 1"},
		},
		{
			name: "excludes boundary commit itself",
			listCommits: []commitSpec{
				{sha: "new-1", message: "new commit 1"},
				{sha: "prev-release-head", message: "previous release head"},
				{sha: "old-1", message: "old commit 1"},
			},
			excludeFromSHA:   "prev-release-head",
			expectedMessages: []string{"new commit 1"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()

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
									"totalCount": 300,
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
				_ = json.NewEncoder(w).Encode(res)
			})

			mux.HandleFunc("/api/v3/repos/o/r/pulls/1", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{
					"commits": 300,
					"head": {"sha": "headsha"},
					"base": {"ref": "master"}
				}`))
			})

			mux.HandleFunc("/api/v3/repos/o/r/commits", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")

				commits := make([]map[string]interface{}, 0, len(tc.listCommits))
				for _, c := range tc.listCommits {
					commits = append(commits, map[string]interface{}{
						"sha": c.sha,
						"commit": map[string]interface{}{
							"message": c.message,
						},
					})
				}

				_ = json.NewEncoder(w).Encode(commits)
			})

			ts := httptest.NewTLSServer(mux)
			defer ts.Close()

			u, _ := url.Parse(ts.URL)
			g := githubGateway{
				domain: u.Host,
			}

			ctx := context.Background()
			ctx = context.WithValue(ctx, prchecklist.ContextKeyHTTPClient, ts.Client())

			ref := prchecklist.ChecklistRef{Owner: "o", Repo: "r", Number: 1}
			commits, err := g.getCommitsByListCommits(ctx, ref, tc.excludeFromSHA)

			require.NoError(t, err)
			require.Len(t, commits, len(tc.expectedMessages))

			for i, msg := range tc.expectedMessages {
				assert.Equal(t, msg, commits[i].Message)
			}
		})
	}
}
