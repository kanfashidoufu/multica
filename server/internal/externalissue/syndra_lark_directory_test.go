package externalissue

import "testing"

func TestSyndraDirectoryRoutes(t *testing.T) {
	for env, base := range map[string]string{
		"prod": "https://syndra.lggj.work/api", "test": "https://test-syndra.lggj.work/api",
		"dev": "https://dev-syndra.lggj.work/api", "pre": "https://pre-api.qingx.cn",
	} {
		if got := syndraDirectoryURL(env); got != base+"/open/multica/version-bugs/assignee-identity" {
			t.Errorf("%s: %s", env, got)
		}
	}
	for _, env := range []string{"", "unknown", "local", "https://example.com", "prod/../../test"} {
		if got := syndraDirectoryURL(env); got != "" {
			t.Errorf("unexpected destination: %s", got)
		}
	}
}

func TestSyndraDirectoryProofContract(t *testing.T) {
	// Shared fixture with the Python resolver, including a UTF-8 employee name.
	if got := syndraDirectoryProof("webhook", "response", "test", 42, 17, "tenant", "employee", "王凝"); got != "3a21614b8be7836cb2248dba1aeaf6908fb8b121fe02aed8807285ec7b85510c" {
		t.Fatal("directory signature contract changed")
	}
}
