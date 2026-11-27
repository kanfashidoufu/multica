package externalissue

import (
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"
)

// FeishuBranchEvidence grounds an expert interpretation in the bound member's current DM.
type FeishuBranchEvidence struct {
	// UserText and PriorOffer are read from Feishu using the frozen task delivery, not supplied by the agent.
	UserText, PriorOffer, IssueKey, Fragment string
	SourceTaskID                             pgtype.UUID
	AmbiguousScope                           bool
}

var offerWords = regexp.MustCompile("[\\s`*，。；！？、（）()「」“”\\[\\]<>:：]+")

func (e FeishuBranchEvidence) AllowsIssue(prefix string, unique bool) bool {
	ref := regexp.MustCompile(`\b` + regexp.QuoteMeta(prefix) + `-[0-9]+\b`)
	if refs := ref.FindAllString(e.UserText, -1); len(refs) > 0 {
		for _, r := range refs {
			if r != e.IssueKey {
				return false
			}
		}
		return true
	}
	if unique {
		return true
	}
	refs := ref.FindAllString(e.PriorOffer, -1)
	return len(refs) > 0 && !slices.ContainsFunc(refs, func(id string) bool { return id != e.IssueKey })
}

func (e *FeishuBranchEvidence) supports(branch string, candidates []string) bool {
	// The agent interprets intent. It cannot invent the user's words, submit a
	// different candidate, or turn a bare "yes" into an arbitrary choice.
	if e.AmbiguousScope || strings.TrimSpace(e.Fragment) == "" || strings.ContainsAny(e.UserText, "?？") {
		return false
	}
	var supporting []string
	for _, clause := range strings.FieldsFunc(e.UserText, func(r rune) bool { return strings.ContainsRune("，,；;。\n", r) }) {
		if strings.Contains(clause, e.Fragment) && !uncertainSelection(clause) {
			supporting = append(supporting, clause)
		}
	}
	if len(supporting) == 0 {
		return false
	}
	if len(candidates) > 0 && !slices.Contains(candidates, branch) {
		return false
	}
	if slices.Contains(offerWords.Split(strings.Join(supporting, " "), -1), branch) {
		return true
	}
	if utf8.RuneCountInString(path.Base(e.Fragment)) >= 2 && strings.Contains(path.Base(branch), path.Base(e.Fragment)) && strings.Contains(branch, e.Fragment) && slices.Contains(candidates, branch) {
		hits := 0
		for _, b := range candidates {
			if strings.Contains(b, e.Fragment) {
				hits++
			}
		}
		if hits == 1 {
			return true
		}
	}
	words := offerWords.Split(e.PriorOffer, -1)
	if uncertainSelection(e.UserText) || !slices.Contains(words, e.IssueKey) || !slices.Contains(words, branch) {
		return false
	}
	// A status update is not an offer. An ambiguous answer goes back to the
	// conversation instead of changing a personal default.
	var shown []string
	for _, word := range words {
		if slices.Contains(candidates, word) && !slices.Contains(shown, word) {
			shown = append(shown, word)
		}
	}
	if idx := displayedOrdinal(e.Fragment); idx >= 0 {
		return idx < len(shown) && shown[idx] == branch
	}
	return len(shown) == 1 && strings.ContainsAny(e.PriorOffer, "?？") && affirmedOffer(e.UserText)
}

// Resolve ordinals only against a list the bot actually showed in this DM,
// not against Git sort order or a guessed proposal from the model.
var ordinal = regexp.MustCompile(`^第([一二三四五六七八九十]|[1-9][0-9]?)[个条项]?$`)

func displayedOrdinal(fragment string) int {
	m := ordinal.FindStringSubmatch(fragment)
	if m == nil {
		return -1
	}
	if n, err := strconv.Atoi(m[1]); err == nil {
		return n - 1
	}
	return slices.Index([]rune("一二三四五六七八九十"), []rune(m[1])[0])
}
func affirmedOffer(text string) bool {
	// Only the context-only path needs a sign of assent; named choices are
	// grounded independently. The user need not repeat an exact command.
	text = strings.ToLower(strings.TrimSpace(text))
	for _, s := range []string{"可以", "同意", "没问题", "没错", "确认", "就这个", "就这条", "就用", "就按", "按这个", "照这个", "照这个来", "好啊", "行的", "当然", "是的", "好的", "yes", "ok"} {
		if strings.Contains(text, s) {
			return true
		}
	}
	return strings.Trim(text, "。.!！") == "对" || strings.Trim(text, "。.!！") == "好" || strings.Trim(text, "。.!！") == "是"
}

func uncertainSelection(text string) bool {
	if strings.ContainsAny(text, "?？") {
		return true
	}
	for _, v := range []string{"不要", "别用", "不是", "不确定", "不用", "不同意", "不可以", "不确认", "先别", "先不", "再说", "晚点", "先等等", "不行", "还没", "能否", "是否", "能不能", "可不可以", "don't", "do not"} {
		if strings.Contains(strings.ToLower(text), v) {
			return true
		}
	}
	return false
}
