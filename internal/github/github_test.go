package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

func init() {
	if os.Getenv("AIH_GH_HELPER") != "1" {
		return
	}
	joined := strings.Join(os.Args[1:], " ")
	if strings.Contains(joined, "--paginate") {
		if os.Getenv("AIH_GH_MODE") == "existing" {
			fmt.Print(`[{"number":3,"body":"first"}][{"number":7,"body":"<!-- aih:stable -->"}]`)
		} else {
			fmt.Print(`[]`)
		}
	} else if strings.Contains(joined, "--method POST") {
		var b map[string]any
		data, _ := io.ReadAll(os.Stdin)
		if json.Unmarshal(data, &b) != nil || b["title"] == nil || b["body"] == nil {
			os.Exit(2)
		}
		fmt.Print(`{"number":9}`)
	} else if strings.Contains(joined, "/pulls?") {
		fmt.Print(`[]`)
	} else if strings.Contains(joined, "/pulls/9") {
		fmt.Print(`{"number":9,"state":"open","head":{"sha":"head","ref":"aih/task"},"base":{"ref":"main"}}`)
	} else {
		fmt.Print(`{"has_issues":true,"archived":false,"permissions":{"push":true}}`)
	}
	os.Exit(0)
}
func TestGHProtocolPaginationAndIdempotency(t *testing.T) {
	t.Setenv("AIH_GH_HELPER", "1")
	t.Setenv("AIH_GH_MODE", "existing")
	exe, _ := os.Executable()
	c := Client{Repo: "owner/repo", Executable: exe}
	ctx := context.Background()
	if e := c.Capabilities(ctx); e != nil {
		t.Fatal(e)
	}
	n, e := c.EnsureIssue(ctx, "stable", "Title", "Body")
	if e != nil || n != 7 {
		t.Fatal("marker reconciliation failed", n, e)
	}
	t.Setenv("AIH_GH_MODE", "new")
	n, e = c.EnsureIssue(ctx, "new", "Title", "Body")
	if e != nil || n != 9 {
		t.Fatal("structured issue creation failed", n, e)
	}
	n, e = c.EnsurePR(ctx, "aih/task", "main", "Title", "Evidence")
	if e != nil || n != 9 {
		t.Fatal("structured PR creation failed", n, e)
	}
	p, e := c.Pull(ctx, 9)
	if e != nil || p.Head.Ref != "aih/task" || p.Base.Ref != "main" {
		t.Fatal(p, e)
	}
}
