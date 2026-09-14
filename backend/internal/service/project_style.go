package service

import (
	"os/exec"
	"strings"
	"unicode"
)

// commitLangSampleSize is how many recent non-merge commit subjects we sample
// when detecting the project's dominant language. Larger than the typical
// "read 50 commits" heuristic so a noisy first few hundred don't dominate a
// 10k-commit OSS repo, but small enough that `git log` on a slow filesystem
// still finishes in well under a second.
const commitLangSampleSize = 500

// commitLangMinChars is the floor on total valid (CJK + Latin) characters
// before we trust the ratio. Below this we don't have enough signal to tell
// "Chinese-heavy" from "all-emoji" and fall back to the default per the
// requirement "在无法确定项目历史风格的情况下，相关内容使用英文".
const commitLangMinChars = 10

// commitLangThreshold is the proportion (0..1) at which we declare a single
// language dominant. Anything below this on both sides becomes "mixed".
const commitLangThreshold = 0.7

// mergeBotPrefixes lists subject prefixes we skip before tallying. These come
// from Dependabot / Renovate / GitHub UI merge buttons and don't reflect the
// project's authored style.
var mergeBotPrefixes = []string{
	"Merge ",
	"Revert ",
	"Bump ",
	"chore(deps",
	"build(deps",
	"ci(deps",
}

// DetectCommitLanguage reads up to commitLangSampleSize non-merge commits from
// the project at projPath, tallies CJK (unicode.Han) vs Latin (ASCII letters)
// characters, and returns the dominant style.
//
//   - (lang, hash, nil) on success — lang ∈ {"zh", "en", "mixed"}
//   - ("en", "", nil) on git error or insufficient samples (the requirement
//     mandates English as the safe default when style is undetermined)
//   - ("en", "", err) only on truly catastrophic failures (we still default
//     to English so the caller never blocks a scan)
//
// hash is the SHA256 hex of the concatenated subjects (in log order). The
// scanner uses it as the "did anything new land?" sentinel.
func DetectCommitLanguage(projPath string) (string, string, error) {
	if projPath == "" {
		return "en", "", nil
	}
	cmd := exec.Command("git", "-C", projPath, "log",
		"--no-merges",
		"-n", itoa(commitLangSampleSize),
		"--pretty=format:%s")
	out, err := cmd.Output()
	if err != nil {
		// not a git repo, git not installed, or no commits — treat as
		// undetermined and default to English per the requirement.
		return "en", "", nil
	}

	raw := strings.Split(string(out), "\n")
	samples := make([]string, 0, len(raw))
	var cjk, latin int
	for _, line := range raw {
		s := strings.TrimSpace(line)
		if s == "" {
			continue
		}
		if hasAnyPrefix(s, mergeBotPrefixes) {
			continue
		}
		samples = append(samples, s)
		for _, r := range s {
			switch {
			case unicode.Is(unicode.Han, r):
				cjk++
			case r < 128 && unicode.IsLetter(r):
				latin++
			}
		}
	}

	total := cjk + latin
	if total < commitLangMinChars {
		// Too few alphabetic characters to be statistically meaningful;
		// the file probably contains only symbols / numbers / punctuation
		// (e.g. a release commit that bumps a version). Default to English.
		return "en", "", nil
	}

	var lang string
	cjkRatio := float64(cjk) / float64(total)
	latinRatio := float64(latin) / float64(total)
	switch {
	case cjkRatio > commitLangThreshold:
		lang = "zh"
	case latinRatio > commitLangThreshold:
		lang = "en"
	default:
		lang = "mixed"
	}
	return lang, sha256Hex(strings.Join(samples, "\n")), nil
}

// StyleHint maps a language code to the LLM-facing instruction string
// spliced into the push sub-task prompt and the pr_author system prompt.
//
//   - "zh"     → 强制中文撰写
//   - "en"     → 强制英文撰写
//   - 其它(包含 "mixed" / "auto" / "") → 默认英文，但说明项目是中英混用
func StyleHint(lang string) string {
	switch lang {
	case "zh":
		return "请使用中文撰写 commit 信息与 PR 标题/正文。"
	case "en":
		return "Please write commit messages and PR title/body in English."
	default:
		return "项目历史风格为中英混用；请默认使用英文撰写 commit 信息与 PR 标题/正文。"
	}
}

// ResolveCommitLang picks the effective language for a project using the
// precedence: non-empty override > detected stored value > "en" default.
//
// Pass project.CommitLang and project.CommitLangOverride directly. The
// "auto" string is never returned — it's the API-level "no override" marker
// that callers may store as "" in the override column.
func ResolveCommitLang(stored, override string) string {
	if override != "" && override != "auto" {
		return override
	}
	if stored != "" && stored != "auto" {
		return stored
	}
	return "en"
}

// CommitLangEnumValues returns the four values the UI is allowed to pick from
// in the override selector. Order matters — the first entry ("auto") is the
// default state meaning "no override, use the detected value".
func CommitLangEnumValues() []string {
	return []string{"auto", "zh", "en", "mixed"}
}

// hasAnyPrefix returns true when s starts with any of the supplied prefixes.
// Inline copy of strings.HasPrefix over a slice; cheaper than building a
// trie for the half-dozen entries we have today.
func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// itoa converts a non-negative int to its decimal string form without
// pulling in strconv for a single call site.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
