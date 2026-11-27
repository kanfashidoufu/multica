package externalissue

import "testing"

func TestFeishuBranchEvidence(t *testing.T) {
	refs := []string{"feature/v3.1609_大润发一二期整合", "feature/v3.1609_大润发二期验收"}
	const key = "QTB-1974"
	offer := key + " 用 " + refs[0] + "，是这条吗？"
	ordered := key + " 选哪条？\n1. " + refs[1] + "\n2. " + refs[0]
	for _, c := range []struct {
		user, fragment, offer string
		want                  bool
	}{
		{"open-platform 用一二期整合那条", "一二期整合", "", true},
		{"不是验收那条，选一二期整合", "一二期整合", "", true},
		{"选大润发", "大润发", "", false},
		{"不要一二期整合", "一二期整合", "", false},
		{"一二期整合能用吗？", "一二期整合", "", false},
		{"帮我查进度", "一二期整合", "", false},
		{"好的，那就用你说的这个", "好的", offer, true},
		{"好的", "好的", "", false},
		{"好的", "好的", "QTB-19740 用 " + refs[0] + " 吗？", false},
		{"用第二条吧", "第二条", ordered, true},
		{"用第一条吧", "第一条", ordered, false},
		{"用第二条吧", "第二条", "", false},
		{"好的", "好的", ordered, false},
	} {
		e := FeishuBranchEvidence{UserText: c.user, Fragment: c.fragment, PriorOffer: c.offer, IssueKey: key}
		if got := e.supports(refs[0], refs); got != c.want {
			t.Errorf("%q %q: got %v", c.user, c.fragment, got)
		}
	}
}
