package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/nvrakesh06/ai-agentic-harness/internal/platform"
	"github.com/nvrakesh06/ai-agentic-harness/internal/safety"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

type Issue struct {
	Number int    `json:"number"`
	Body   string `json:"body"`
	State  string `json:"state"`
}
type Pull struct {
	Number int    `json:"number"`
	State  string `json:"state"`
	Merged bool   `json:"merged"`
	Head   struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"base"`
}
type Service interface {
	EnsureIssue(context.Context, string, string, string) (int, error)
	UpdateIssue(context.Context, int, string, bool) error
	EnsurePR(context.Context, string, string, string, string) (int, error)
	UpdatePR(context.Context, int, string) error
	Pull(context.Context, int) (Pull, error)
	Issues(context.Context) ([]Issue, error)
}
type Client struct {
	Repo       string
	Dir        string
	Executable string
}

func (c Client) run(ctx context.Context, input string, args ...string) (string, error) {
	if c.Executable == "" {
		c.Executable = "gh"
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out, e := platform.Run(ctx, c.Dir, append(os.Environ(), "GH_PROMPT_DISABLED=1"), input, c.Executable, args...)
	if e != nil {
		return "", fmt.Errorf("GitHub operation failed: %w: %s", e, safety.Redact(out))
	}
	return out, nil
}
func (c Client) api(ctx context.Context, method, endpoint string, body any, out any) error {
	args := []string{"api", "--method", method, endpoint}
	input := ""
	if body != nil {
		b, e := json.Marshal(body)
		if e != nil {
			return e
		}
		input = string(b)
		if e = safety.Check(input); e != nil {
			return e
		}
		args = append(args, "--input", "-")
	}
	b, e := c.run(ctx, input, args...)
	if e != nil {
		return e
	}
	if out != nil {
		return json.Unmarshal([]byte(b), out)
	}
	return nil
}
func (c Client) Capabilities(ctx context.Context) error {
	var r struct {
		Archived    bool `json:"archived"`
		HasIssues   bool `json:"has_issues"`
		Permissions struct {
			Push bool `json:"push"`
		} `json:"permissions"`
	}
	if e := c.api(ctx, "GET", "repos/"+c.Repo, nil, &r); e != nil {
		return e
	}
	if r.Archived || !r.HasIssues || !r.Permissions.Push {
		return errors.New("AIH requires an unarchived repository with issues enabled and write access")
	}
	return nil
}
func (c Client) Issues(ctx context.Context) ([]Issue, error) {
	b, e := c.run(ctx, "", "api", "--paginate", "repos/"+c.Repo+"/issues?state=all&per_page=100")
	if e != nil {
		return nil, e
	}
	d := json.NewDecoder(strings.NewReader(b))
	var out []Issue
	for {
		var page []Issue
		e = d.Decode(&page)
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, e
		}
		out = append(out, page...)
	}
	return out, nil
}
func Marker(key string) string { return "<!-- aih:" + key + " -->" }
func (c Client) EnsureIssue(ctx context.Context, key, title, body string) (int, error) {
	issues, e := c.Issues(ctx)
	if e != nil {
		return 0, e
	}
	for _, i := range issues {
		if strings.Contains(i.Body, Marker(key)) {
			return i.Number, nil
		}
	}
	var i Issue
	e = c.api(ctx, "POST", "repos/"+c.Repo+"/issues", map[string]any{"title": title, "body": Marker(key) + "\n\n" + body}, &i)
	return i.Number, e
}
func (c Client) UpdateIssue(ctx context.Context, n int, body string, closed bool) error {
	state := "open"
	if closed {
		state = "closed"
	}
	return c.api(ctx, "PATCH", "repos/"+c.Repo+"/issues/"+strconv.Itoa(n), map[string]any{"body": body, "state": state}, nil)
}
func (c Client) EnsurePR(ctx context.Context, branch, base, title, body string) (int, error) {
	owner := strings.Split(c.Repo, "/")[0]
	var list []Pull
	if e := c.api(ctx, "GET", "repos/"+c.Repo+"/pulls?state=all&head="+owner+":"+branch, nil, &list); e != nil {
		return 0, e
	}
	if len(list) > 0 {
		if list[0].State == "closed" && !list[0].Merged {
			return 0, errors.New("task PR was closed without merge; human decision required")
		}
		return list[0].Number, nil
	}
	var p Pull
	e := c.api(ctx, "POST", "repos/"+c.Repo+"/pulls", map[string]any{"head": branch, "base": base, "title": title, "body": body}, &p)
	return p.Number, e
}
func (c Client) UpdatePR(ctx context.Context, n int, body string) error {
	return c.api(ctx, "PATCH", "repos/"+c.Repo+"/pulls/"+strconv.Itoa(n), map[string]any{"body": body}, nil)
}
func (c Client) Pull(ctx context.Context, n int) (Pull, error) {
	var p Pull
	e := c.api(ctx, "GET", "repos/"+c.Repo+"/pulls/"+strconv.Itoa(n), nil, &p)
	return p, e
}
