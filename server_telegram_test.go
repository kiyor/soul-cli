package main

import "testing"

func TestTrimWhisperTailHallucinations(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain text", "今天天气真好", "今天天气真好"},
		{"trailing 谢谢大家", "今天我们来讨论 Kubernetes，谢谢大家", "今天我们来讨论 Kubernetes"},
		{"trailing 谢谢大家 with period", "讨论一下问题。谢谢大家。", "讨论一下问题"},
		{"trailing 谢谢观看", "好，就这些。谢谢观看", "好，就这些"},
		{"traditional 謝謝大家", "今天就到这里 謝謝大家", "今天就到这里"},
		{"stacked 谢谢大家 + 谢谢观看", "辛苦了。谢谢大家。谢谢观看。", "辛苦了"},
		{"stacked with whitespace", "完了 谢谢大家 谢谢观看 ", "完了"},
		{"long YouTube outro", "今天讨论的就这些。请不吝点赞订阅转发打赏支持明镜与点点栏目", "今天讨论的就这些"},
		{"Amara subtitle credit", "好的就这样。字幕由 Amara.org 社区提供", "好的就这样"},
		{"middle 谢谢大家 should stay", "我说谢谢大家然后他们就走了", "我说谢谢大家然后他们就走了"},
		{"only hallucination", "谢谢大家", ""},
		{"only stacked hallucinations", "谢谢大家。谢谢观看", ""},
		{"english thanks for watching", "And that's a wrap. Thanks for watching", "And that's a wrap"},
		{"trim whitespace tail", "正常内容   ", "正常内容"},
		{"comma before hallucination", "OK，谢谢大家", "OK"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := trimWhisperTailHallucinations(tc.in)
			if got != tc.want {
				t.Errorf("trimWhisperTailHallucinations(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
