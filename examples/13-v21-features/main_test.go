package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRunFeature_Teams(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	err := run(context.Background(), []string{"-feature", "teams"}, &out)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out.String(), "teams demo: OK") {
		t.Fatalf("unexpected output: %q", out.String())
	}
}
