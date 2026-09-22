// checkfmt makes formatting validation identical on Windows and Unix.
package main

import (
	"bytes"
	"fmt"
	"go/format"
	"os"
	"os/exec"
	"strings"
)

func main() {
	out, err := exec.Command("git", "ls-files", "--cached", "--others", "--exclude-standard", "-z", "--", "*.go").Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	failed := false
	for _, path := range strings.Split(string(out), "\x00") {
		if path == "" {
			continue
		}
		data, err := os.ReadFile(path)
		if err == nil {
			var formatted []byte
			formatted, err = format.Source(data)
			if err == nil && !bytes.Equal(bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n")), formatted) {
				err = fmt.Errorf("run gofmt -w %s", path)
			}
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, path, err)
			failed = true
		}
	}
	if failed {
		os.Exit(1)
	}
}
