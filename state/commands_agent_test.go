package state

import "testing"

// 旧命令名は allowlist に残っていなければならない（先行デプロイの前提）。
// 外すと、まだ更新していない PC の Web/CLI から命令を投げられなくなる。
func TestLegacyCommandNamesStillValid(t *testing.T) {
	for _, c := range []string{"restart-claude", "update-claude", "restart-agent",
		"self-update", "restart-proxy", "update-all"} {
		if !ValidCommands[c] {
			t.Errorf("旧命令 %q が allowlist から消えた（旧 PC から投入不能になる）", c)
		}
	}
	for _, c := range []string{"restart-agent-session", "update-agent-cli", "restart-daemon"} {
		if !ValidCommands[c] {
			t.Errorf("新命令 %q が allowlist に無い", c)
		}
	}
}

// 旧名は claude 固定へ写像する（旧名は claude 専用だった＝推測ではなく事実）。
// restart-agent だけは元々 agent 概念を持たない（drover デーモンの再起動）。
func TestNormalizeCommand(t *testing.T) {
	for _, tc := range []struct {
		cmd, agent string
		wantCmd    string
		wantAgent  string
		wantLegacy bool
	}{
		{"restart-claude", "", "restart-agent-session", "claude", true},
		{"update-claude", "", "update-agent-cli", "claude", true},
		{"restart-agent", "", "restart-daemon", "", true},
		{"restart-agent-session", "codex", "restart-agent-session", "codex", false},
		{"update-all", "", "update-all", "", false},
		{"self-update", "", "self-update", "", false},
	} {
		gc, ga, gl := NormalizeCommand(tc.cmd, tc.agent)
		if gc != tc.wantCmd || ga != tc.wantAgent || gl != tc.wantLegacy {
			t.Errorf("NormalizeCommand(%q,%q) = (%q,%q,%v), want (%q,%q,%v)",
				tc.cmd, tc.agent, gc, ga, gl, tc.wantCmd, tc.wantAgent, tc.wantLegacy)
		}
	}
}

// ValidAgents は herdr の canonical 21 種。agent 側の agentid.CanonicalLabels と
// 同じ集合でなければ「投入できるが誰も拾わない」命令が生まれる。
func TestValidAgentsIsCanonical21(t *testing.T) {
	if len(ValidAgents) != 21 {
		t.Fatalf("ValidAgents = %d 種, want 21（herdr detect/mod.rs agent_label）", len(ValidAgents))
	}
	for _, a := range []string{"claude", "codex", "cursor", "gemini", "qodercli"} {
		if !ValidAgents[a] {
			t.Errorf("canonical label %q が欠けている", a)
		}
	}
	for _, a := range []string{"claude-code", "cursor-agent", "kilo-code", ""} {
		if ValidAgents[a] {
			t.Errorf("%q は lookup_agent の入力 alias であって canonical ではない", a)
		}
	}
}
