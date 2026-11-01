package prompt

import (
	"strings"
	"testing"
)

// TestBuildStageQAPrompt_ResumedForm pins the short form: the question plus
// the answering rules, and NOT the design doc (the session already holds it).
func TestBuildStageQAPrompt_ResumedForm(t *testing.T) {
	p := BuildStageQAPrompt(StageQAPromptInput{
		Resumed: true,
		Stage:   "design",
		Question: "为什么用 turn 表而不是转录表？",
	})

	if !strings.Contains(p, "为什么用 turn 表而不是转录表？") {
		t.Fatal("the question is missing from the prompt")
	}
	// The resumed form must NOT re-paste the plan — that would burn context
	// for nothing, since the forked session already holds it.
	if strings.Contains(p, "技术方案（原文）") {
		t.Fatal("the resumed form must not paste the design doc")
	}
	if strings.Contains(p, "需求上下文") {
		t.Fatal("the resumed form must not paste the requirement context block")
	}
	// The read-only contract must be there in BOTH forms.
	for _, want := range []string{"只答疑", "不要修改任何文件", "不要输出实施计划"} {
		if !strings.Contains(p, want) {
			t.Fatalf("resumed prompt is missing %q", want)
		}
	}
}

// TestBuildStageQAPrompt_FreshForm pins the self-contained form: it must
// carry the requirement context AND the earlier Q&A, because the session it
// runs in holds none of it.
func TestBuildStageQAPrompt_FreshForm(t *testing.T) {
	p := BuildStageQAPrompt(StageQAPromptInput{
		Resumed:     false,
		Stage:       "design",
		Title:       "增加阶段答疑",
		Description: "针对方案 / 开发会话的答疑对话",
		DesignDocs:  "# 技术实现方案\n\n## 一、需求理解\n\n blah",
		Question:    "为什么这里用 run 表？",
		History: []QATurn{
			{Question: "上一个问题", Answer: "上一个答案"},
		},
	})

	for _, want := range []string{
		"增加阶段答疑",
		"针对方案 / 开发会话的答疑对话",
		"技术实现方案",
		"上一个问题",
		"上一个答案",
		"为什么这里用 run 表？",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("fresh prompt is missing %q", want)
		}
	}
	for _, want := range []string{"只答疑", "不要修改任何文件"} {
		if !strings.Contains(p, want) {
			t.Fatalf("fresh prompt is missing the read-only rule %q", want)
		}
	}
}

// TestBuildStageQAPrompt_CodingStage checks the coding form carries the
// coding summary (there is no "plan document" for a coding run).
func TestBuildStageQAPrompt_CodingStage(t *testing.T) {
	p := BuildStageQAPrompt(StageQAPromptInput{
		Stage:         "coding",
		Title:         "增加阶段答疑",
		CodingSummary: "已改：backend/internal/handler/wizard_qa.go",
		Question:      "这段为什么用 JobStore？",
	})
	if !strings.Contains(p, "开发阶段上下文摘要") {
		t.Fatal("the coding form must carry the coding summary block")
	}
	if !strings.Contains(p, "已改：backend/internal/handler/wizard_qa.go") {
		t.Fatal("the coding summary body is missing")
	}
	if !strings.Contains(p, "开发者") {
		t.Fatal("the coding form must address the developer role")
	}
}

// TestBuildStageQAPrompt_TruncatesByRune guards the rune-based truncation:
// a byte cut would split a Chinese character and emit invalid UTF-8 into the
// prompt.
func TestBuildStageQAPrompt_TruncatesByRune(t *testing.T) {
	long := strings.Repeat("答", qaDesignMaxRunes+500)
	p := BuildStageQAPrompt(StageQAPromptInput{
		Title:      "标题",
		DesignDocs: long,
		Question:   "问题",
	})
	if !strings.Contains(p, "已截断") {
		t.Fatal("a truncated design doc must leave a visible breadcrumb")
	}
	// The pasted doc must be shorter than the original by a wide margin.
	if strings.Count(p, "答") > qaDesignMaxRunes+100 {
		t.Fatalf("the design doc was not truncated: %d chars pasted", strings.Count(p, "答"))
	}
}

// TestBuildStageQAPrompt_TruncatesHistoryAnswer keeps one long earlier answer
// from crowding out the question.
func TestBuildStageQAPrompt_TruncatesHistoryAnswer(t *testing.T) {
	long := strings.Repeat("x", qaAnswerMaxRunes+100)
	p := BuildStageQAPrompt(StageQAPromptInput{
		Question: "当前问题",
		History:  []QATurn{{Question: "旧问题", Answer: long}},
	})
	if strings.Count(p, "x") > qaAnswerMaxRunes+100 {
		t.Fatalf("the earlier answer was not truncated: %d chars", strings.Count(p, "x"))
	}
	if !strings.Contains(p, "当前问题") {
		t.Fatal("the current question must survive the history replay")
	}
}
