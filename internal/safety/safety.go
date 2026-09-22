package safety

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

var secrets = regexp.MustCompile(`(?i)(-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----|\b(?:gh[pousr]_[a-zA-Z0-9]{20,}|github_pat_[a-zA-Z0-9_]{20,}|sk-[a-zA-Z0-9_-]{20,}|AKIA[A-Z0-9]{16})\b|(?:api[_-]?key|password|secret|token)["']?\s*[:=]\s*["']?[a-zA-Z0-9_+/=-]{24,})`)

func Check(data string) error {
	if secrets.MatchString(data) {
		return fmt.Errorf("possible secret detected; publication blocked (value redacted)")
	}
	return nil
}
func Redact(data string) string { return secrets.ReplaceAllString(data, "[REDACTED]") }

// Runtime diagnostics may mention paths. Replace known local roots before any
// diagnostic is copied to portable task state or a GitHub issue.
func Portable(data string, roots ...string) string {
	data = Redact(data)
	for _, root := range roots {
		if root == "" {
			continue
		}
		for _, form := range []string{root, strings.ReplaceAll(root, `\`, "/"), strings.ReplaceAll(root, `\`, `\\`)} {
			data = strings.ReplaceAll(data, form, "<local>")
		}
	}
	return data
}
func Path(name string) error {
	p := strings.ToLower(strings.ReplaceAll(name, "\\", "/"))
	b := path.Base(p)
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "../") || strings.Contains(p, ":") || b == ".env" || (strings.HasPrefix(b, ".env.") && b != ".env.example" && b != ".env.sample") || b == "id_rsa" || b == "id_ed25519" || strings.HasSuffix(b, ".pem") || strings.HasSuffix(b, ".key") || strings.HasSuffix(b, ".db") || strings.Contains(p, "/.git/") {
		return fmt.Errorf("sensitive or invalid checkpoint path %q", name)
	}
	return nil
}
