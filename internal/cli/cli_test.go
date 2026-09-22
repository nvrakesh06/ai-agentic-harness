package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestInspectionWithoutProject(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{{[]string{"version"}, "AIH 1.0.0"}, {[]string{"rules", "--core"}, "Understand before changing"}, {[]string{"--help"}, "handoff"}, {[]string{"roles", "assign", "--help"}, "task-id"}} {
		var b bytes.Buffer
		c := New()
		c.SetOut(&b)
		c.SetErr(&b)
		c.SetArgs(tc.args)
		if e := c.ExecuteContext(context.Background()); e != nil {
			t.Fatal(tc.args, e)
		}
		if !strings.Contains(b.String(), tc.want) {
			t.Fatal(tc.args, b.String())
		}
	}
}
