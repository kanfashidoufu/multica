package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReleaseSourceDefaultsToGitHub(t *testing.T) {
	t.Setenv(releaseAPIBaseURLEnv, "")
	t.Setenv(releaseDownloadBaseURLEnv, "")

	if got := releaseAPIBaseURL(); got != defaultReleaseAPIBaseURL {
		t.Fatalf("release API base URL = %q, want %q", got, defaultReleaseAPIBaseURL)
	}
	if got := releaseAssetDownloadURL("archive.tar.gz", "v1.2.3", "https://github.example/archive.tar.gz"); got != "https://github.example/archive.tar.gz" {
		t.Fatalf("fallback download URL = %q", got)
	}
}

func TestReleaseSourceOverridesGitHub(t *testing.T) {
	t.Setenv(releaseAPIBaseURLEnv, "https://mirror.example/api/")
	t.Setenv(releaseDownloadBaseURLEnv, "https://mirror.example/releases/")

	if got := releaseAPIBaseURL(); got != "https://mirror.example/api" {
		t.Fatalf("release API base URL = %q", got)
	}
	if got := releaseAssetDownloadURL("multica-cli-1.2.3-linux-amd64.tar.gz", "v1.2.3", "https://github.example/archive.tar.gz"); got != "https://mirror.example/releases/v1.2.3/multica-cli-1.2.3-linux-amd64.tar.gz" {
		t.Fatalf("mirror download URL = %q", got)
	}
}

func TestReleaseFetchUsesConfiguredAPIAndRepository(t *testing.T) {
	for _, tc := range []struct {
		name, repository, githubRepository, want string
	}{
		{"localized default", "", "", "kanfashidoufu/multica"},
		{"repository override", "example/private-multica", "ignored/repo", "example/private-multica"},
		{"github repository fallback", "", "example/fork", "example/fork"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MULTICA_RELEASE_REPOSITORY", tc.repository)
			t.Setenv("GITHUB_REPOSITORY", tc.githubRepository)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				prefix := "/api/repos/" + tc.want + "/releases/"
				if r.URL.EscapedPath() != prefix+"latest" && r.URL.EscapedPath() != prefix+"tags/v9.8.7%2Fpreview" {
					t.Errorf("unexpected release request: %s", r.URL.EscapedPath())
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(GitHubRelease{TagName: "v9.8.7"})
			}))
			defer server.Close()
			t.Setenv(releaseAPIBaseURLEnv, server.URL+"/api/")

			for _, fetch := range []func() (*GitHubRelease, error){
				FetchLatestRelease,
				func() (*GitHubRelease, error) { return fetchReleaseByTag("v9.8.7/preview") },
			} {
				got, err := fetch()
				if err != nil {
					t.Fatalf("fetch release: %v", err)
				}
				if got.TagName != "v9.8.7" {
					t.Fatalf("release tag = %q, want v9.8.7", got.TagName)
				}
			}
		})
	}
}
