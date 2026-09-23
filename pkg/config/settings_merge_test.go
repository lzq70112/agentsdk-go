package config

import "testing"

func TestMergeSettings(t *testing.T) {
	t.Parallel()

	lower := &Settings{
		APIKeyHelper: "low",
		Env:          map[string]string{"A": "1"},
		Permissions:  &PermissionsConfig{Allow: []string{"A"}},
		Sandbox:      &SandboxConfig{Enabled: boolPtr(true)},
		BashOutput:   &BashOutputConfig{SyncThresholdBytes: intPtr(1)},
		StreamStall:  &StreamStallConfig{Timeout: "60s", FallbackEnabled: boolPtr(true)},
	}
	higher := &Settings{
		APIKeyHelper: "high",
		Env:          map[string]string{"B": "2"},
		Permissions:  &PermissionsConfig{Allow: []string{"B"}, DefaultMode: "ask"},
		Sandbox:      &SandboxConfig{Enabled: boolPtr(false)},
		BashOutput:   &BashOutputConfig{AsyncThresholdBytes: intPtr(2)},
		StreamStall:  &StreamStallConfig{Timeout: "300s", FallbackEnabled: boolPtr(false)},
	}

	merged := MergeSettings(lower, higher)
	if merged.APIKeyHelper != "high" {
		t.Fatalf("unexpected api key helper %q", merged.APIKeyHelper)
	}
	if merged.Env["A"] != "1" || merged.Env["B"] != "2" {
		t.Fatalf("unexpected env %v", merged.Env)
	}
	if merged.Permissions == nil || merged.Permissions.DefaultMode != "ask" {
		t.Fatalf("unexpected permissions %v", merged.Permissions)
	}
	if merged.Sandbox == nil || merged.Sandbox.Enabled == nil || *merged.Sandbox.Enabled {
		t.Fatalf("unexpected sandbox enabled %v", merged.Sandbox)
	}
	if merged.BashOutput == nil || merged.BashOutput.SyncThresholdBytes == nil || merged.BashOutput.AsyncThresholdBytes == nil {
		t.Fatalf("expected bash output merged")
	}
	if merged.StreamStall == nil || merged.StreamStall.Timeout != "300s" || merged.StreamStall.FallbackEnabled == nil || *merged.StreamStall.FallbackEnabled {
		t.Fatalf("expected stream stall merged to higher values, got %v", merged.StreamStall)
	}
}

func TestMergeSettingsNilInputs(t *testing.T) {
	t.Parallel()

	if MergeSettings(nil, nil) != nil {
		t.Fatalf("expected nil merge")
	}
	if MergeSettings(&Settings{APIKeyHelper: "x"}, nil).APIKeyHelper != "x" {
		t.Fatalf("expected lower preserved")
	}
}
