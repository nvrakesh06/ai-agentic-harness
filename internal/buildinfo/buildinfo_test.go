package buildinfo

import (
	"strings"
	"testing"

	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
)

func TestCurrentUsesModelVersionAndSchema(t *testing.T) {
	id := Current()
	if id.Version != model.Version || id.StateSchema != model.StateSchema {
		t.Fatalf("build identity disagrees with runtime: %+v", id)
	}
	if id.Label() == "" {
		t.Fatal("empty build label")
	}
}

func TestLabelBoundsCommitAndMarksDirty(t *testing.T) {
	id := Identity{Version: "v1", StateSchema: 3, Commit: strings.Repeat("a", 40), Dirty: true}
	if got, want := id.Label(), "v1 / aaaaaaaaaaaa+dirty / state 3"; got != want {
		t.Fatalf("label = %q, want %q", got, want)
	}
}
