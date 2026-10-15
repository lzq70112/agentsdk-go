package api

import (
	"strings"
	"testing"
)

func TestBuildEnvironmentSectionBasics(t *testing.T) {
	out := buildEnvironmentSection("D:/Works/demo", "")
	if !strings.Contains(out, "# Environment") {
		t.Fatalf("missing heading: %q", out)
	}
	if !strings.Contains(out, "- Working directory: D:/Works/demo") {
		t.Fatalf("missing working directory: %q", out)
	}
	if !strings.Contains(out, "- Platform: ") {
		t.Fatalf("missing platform: %q", out)
	}
	if !strings.Contains(out, "- Current date: ") {
		t.Fatalf("missing date: %q", out)
	}
	if strings.Contains(out, "Sandbox root") {
		t.Fatalf("sandbox root should be omitted when empty: %q", out)
	}
}

func TestBuildEnvironmentSectionSandboxRoot(t *testing.T) {
	out := buildEnvironmentSection("/proj", "/sandbox")
	if !strings.Contains(out, "- Sandbox root: /sandbox") {
		t.Fatalf("missing sandbox root: %q", out)
	}
}

func TestBuildEnvironmentSectionNonGit(t *testing.T) {
	// pkg/api 下无 .git：应报 false 且不做 git exec。
	out := buildEnvironmentSection(".", "")
	if !strings.Contains(out, "- Is a git repository: false") {
		t.Fatalf("expected non-git report: %q", out)
	}
	if strings.Contains(out, "Main branch") || strings.Contains(out, "Git remote") {
		t.Fatalf("non-git repo should omit git details: %q", out)
	}
}

func TestBuildEnvironmentSectionGitRepo(t *testing.T) {
	dir := t.TempDir()
	if runProbe(dir, "git", "init", "-q") == "" && runProbe(dir, "git", "status") == "" {
		t.Skip("git unavailable")
	}
	out := buildEnvironmentSection(dir, "")
	if !strings.Contains(out, "- Is a git repository: true") {
		t.Fatalf("expected git report: %q", out)
	}
}
