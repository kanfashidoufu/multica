package service

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func syndraSkillScript(t *testing.T, name string) string {
	t.Helper()
	skill, ok := findSkill(t, "multica-fixing-syndra-bugs")
	if !ok {
		t.Fatal("Syndra skill missing")
	}
	for _, f := range skill.Files {
		if f.Path == "scripts/"+name {
			path := filepath.Join(t.TempDir(), name)
			if err := os.WriteFile(path, []byte(f.Content), 0700); err != nil {
				t.Fatal(err)
			}
			return path
		}
	}
	t.Fatalf("script %s missing", name)
	return ""
}

func syndraGit(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func syndraTestRepo(t *testing.T) (string, string, string) {
	t.Helper()
	dir := t.TempDir()
	repo, remote := filepath.Join(dir, "checkout"), filepath.Join(dir, "remote.git")
	syndraGit(t, "init", "--initial-branch=main", repo)
	syndraGit(t, "-C", repo, "config", "user.name", "Multica Test")
	syndraGit(t, "-C", repo, "config", "user.email", "multica-test@example.test")
	syndraGit(t, "-C", repo, "commit", "--allow-empty", "-m", "base")
	syndraGit(t, "init", "--bare", remote)
	syndraGit(t, "-C", repo, "remote", "add", "origin", remote)
	syndraGit(t, "-C", repo, "push", "origin", "HEAD:refs/heads/main")
	return repo, remote, syndraGit(t, "-C", repo, "rev-parse", "HEAD")
}

func TestFindVersionBranchesUsesLiveRemoteAndCompleteVersions(t *testing.T) {
	script := syndraSkillScript(t, "find-version-branches.sh")
	repo, remote, base := syndraTestRepo(t)
	for _, branch := range []string{
		"v2.91.56", "release/v2.91.56_wn", "release/v2.91.56_merge",
		"hotfix/patch-v2.91.56", "release/v2.91.560", "release/v2.91.57",
		"release/v2.91.56.1", "release/v12.91.56", "release/v1.2.91.56",
	} {
		syndraGit(t, "-C", remote, "update-ref", "refs/heads/"+branch, base)
	}
	// Neither stale tracking refs nor tags may become candidates.
	syndraGit(t, "-C", repo, "update-ref", "refs/remotes/origin/deleted-v2.91.56", base)
	syndraGit(t, "-C", remote, "update-ref", "refs/tags/tag-v2.91.56", base)
	for _, tc := range []struct {
		name, version, want string
		fail                bool
	}{
		{"variants", "v2.91.56-企业看板", "version_token=2.91.56\nmatch_count=4\nbranch=hotfix/patch-v2.91.56\nbranch=release/v2.91.56_merge\nbranch=release/v2.91.56_wn\nbranch=v2.91.56\n", false},
		{"none", "v3.0.0", "version_token=3.0.0\nmatch_count=0\n", false},
		{"full version", "v2.91.56.1", "version_token=2.91.56.1\nmatch_count=1\nbranch=release/v2.91.56.1\n", false},
		{"missing", "版本待定", "", true},
		{"ambiguous", "v2.91.56 -> v2.91.57", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := exec.Command("bash", script, tc.version, repo).CombinedOutput()
			if (err != nil) != tc.fail || (!tc.fail && string(out) != tc.want) {
				t.Fatalf("result=%s error=%v, want=%s fail=%v", out, err, tc.want, tc.fail)
			}
		})
	}
	syndraGit(t, "-C", repo, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing.git"))
	out, err := exec.Command("bash", script, "v2.91.56", repo).CombinedOutput()
	if err == nil || strings.Contains(string(out), "match_count=") {
		t.Fatalf("remote failure presented as discovery: %s, %v", out, err)
	}
}

func TestVerifyVersionDeliveryRequiresMergedValidatedHeadInRemoteTarget(t *testing.T) {
	script := syndraSkillScript(t, "verify-version-delivery.sh")
	repo, remote, base := syndraTestRepo(t)
	const branch = "release/v2.91.56"
	const prURL = "https://github.com/example/repo/pull/1"
	// Model a squash: the original PR head is not an ancestor of the merge.
	syndraGit(t, "-C", repo, "commit", "--allow-empty", "-m", "PR fix")
	prHead := syndraGit(t, "-C", repo, "rev-parse", "HEAD")
	syndraGit(t, "-C", repo, "checkout", "--detach", base)
	syndraGit(t, "-C", repo, "commit", "--allow-empty", "-m", "squashed fix")
	merge := syndraGit(t, "-C", repo, "rev-parse", "HEAD")
	syndraGit(t, "-C", repo, "commit", "--allow-empty", "-m", "later release work")
	tip := syndraGit(t, "-C", repo, "rev-parse", "HEAD")
	syndraGit(t, "-C", repo, "push", "origin", "HEAD:refs/heads/"+branch)

	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\nif [ \"$1\" = repo ]; then printf '%s\\n' \"$PR_ORIGIN_URL\"; else cat \"$PR_SNAPSHOT\"; fi\n"), 0700); err != nil {
		t.Fatal(err)
	}
	snapshotPath := filepath.Join(t.TempDir(), "snapshot")
	for _, tc := range []struct {
		name, state, target, head, merged, remoteTip string
		pass                                         bool
	}{
		{"squashed and target advanced", "MERGED", branch, prHead, merge, tip, true},
		{"wrong repository", "MERGED", branch, prHead, merge, tip, false},
		{"queued", "OPEN", branch, prHead, "missing", tip, false},
		{"closed", "CLOSED", branch, prHead, "missing", tip, false},
		{"wrong target", "MERGED", "main", prHead, merge, tip, false},
		{"head changed", "MERGED", branch, base, merge, tip, false},
		{"missing merge", "MERGED", branch, prHead, "missing", tip, false},
		{"remote lost merge", "MERGED", branch, prHead, merge, base, false},
		{"target deleted", "MERGED", branch, prHead, merge, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.remoteTip == "" {
				syndraGit(t, "-C", remote, "update-ref", "-d", "refs/heads/"+branch)
			} else {
				syndraGit(t, "-C", remote, "update-ref", "refs/heads/"+branch, tc.remoteTip)
			}
			snapshot := strings.Join([]string{tc.state, tc.target, tc.head, tc.merged, "2026-09-08T00:00:00Z", prURL}, "\n") + "\n"
			if err := os.WriteFile(snapshotPath, []byte(snapshot), 0600); err != nil {
				t.Fatal(err)
			}
			originURL := "https://github.com/example/repo"
			if tc.name == "wrong repository" {
				originURL = "https://github.com/example/another-repo"
			}
			cmd := exec.Command("bash", script, repo, branch, prURL, prHead)
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "PR_SNAPSHOT="+snapshotPath, "PR_ORIGIN_URL="+originURL)
			out, err := cmd.CombinedOutput()
			if (err == nil) != tc.pass || strings.Contains(string(out), "delivery=merged\n") != tc.pass {
				t.Fatalf("pass=%v output=%s error=%v", tc.pass, out, err)
			}
		})
	}
}

func TestVerifyTestPromotionRequiresVersionTipInRemoteTest(t *testing.T) {
	script := syndraSkillScript(t, "verify-test-promotion.sh")
	repo, _, _ := syndraTestRepo(t)
	syndraGit(t, "-C", repo, "commit", "--allow-empty", "-m", "version merge")
	version := syndraGit(t, "-C", repo, "rev-parse", "HEAD")
	syndraGit(t, "-C", repo, "push", "origin", "HEAD:refs/heads/release/v2.91.56")
	syndraGit(t, "-C", repo, "push", "origin", "HEAD:refs/heads/test")
	good := exec.Command("bash", script, repo, "test", version)
	if out, err := good.CombinedOutput(); err != nil || !strings.Contains(string(out), "promotion=merged\n") {
		t.Fatalf("test promotion should pass: %s, %v", out, err)
	}
	notPromoted := syndraGit(t, "-C", repo, "commit", "--allow-empty", "-m", "unpromoted")
	bad := exec.Command("bash", script, repo, "test", notPromoted)
	if out, err := bad.CombinedOutput(); err == nil || strings.Contains(string(out), "promotion=merged\n") {
		t.Fatalf("unpromoted version tip should fail: %s, %v", out, err)
	}
}

func TestSyndraSkillResourcesAreReachable(t *testing.T) {
	skill, ok := findSkill(t, "multica-fixing-syndra-bugs")
	if !ok {
		t.Fatal("Syndra skill missing")
	}
	// The baseline schema/budget/source-leak tests live in builtin_skills_test.
	reachable := skill.Content
	for _, f := range skill.Files {
		if strings.HasPrefix(f.Path, "references/") {
			reachable += "\n" + f.Content
		}
	}
	for _, f := range skill.Files {
		if !strings.Contains(reachable, f.Path) {
			t.Errorf("entrypoint does not link supporting file %s", f.Path)
		}
	}
}

func TestSyndraSkillRequiresVersionThenTestPromotionAndFailureHandoff(t *testing.T) {
	skill, ok := findSkill(t, "multica-fixing-syndra-bugs")
	if !ok {
		t.Fatal("Syndra skill missing")
	}
	refs := map[string]string{}
	for _, f := range skill.Files {
		refs[f.Path] = f.Content
	}
	delivery := refs["references/version-delivery.md"]
	for _, want := range []string{
		"validated task/PR -> confirmed version branch -> test",
		"git push origin HEAD:refs/heads/test",
		"current human assignee",
		"## Validation failure path",
		"Push the version branch. Do not merge it into `test`",
		"blocked --no-start",
		"not a successful delivery",
	} {
		if !strings.Contains(delivery, want) {
			t.Errorf("version-delivery reference missing %q", want)
		}
	}
}
