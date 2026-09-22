package config

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// UpstreamRepository identifies this software, never the user's application or
// Git identity. Forks can override it without changing portable project policy.
const UpstreamRepository = "nvrakesh06/ai-agentic-harness"

func ReleaseRepository(configured string) string {
	if override := os.Getenv("AIH_RELEASE_REPO"); override != "" {
		return override
	}
	if configured != "" {
		return configured
	}
	return UpstreamRepository
}

var repositoryName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func ValidRepository(name string) bool {
	return repositoryName.MatchString(name) && !strings.Contains(name, "..")
}

// LoadEnvironment is deliberately opt-in. Repository .env files are never
// discovered or executed. Values are literal, with optional matching quotes;
// there is no shell evaluation, interpolation, multiline syntax, or export.
// Existing process variables win, including explicitly empty values.
func LoadEnvironment(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open environment file: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1024*1024 {
		return fmt.Errorf("environment file must be a regular file no larger than 1 MiB")
	}
	values := map[string]string{}
	keyPattern := regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "\ufeff"))
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		key, value, ok := strings.Cut(text, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !ok || !keyPattern.MatchString(key) || strings.ContainsRune(value, 0) {
			return fmt.Errorf("invalid environment assignment on line %d (expected KEY=literal-value)", line)
		}
		if _, duplicate := values[key]; duplicate {
			return fmt.Errorf("duplicate environment key on line %d", line)
		}
		if len(value) > 0 && (value[0] == '\'' || value[0] == '"') {
			if len(value) < 2 || value[len(value)-1] != value[0] {
				return fmt.Errorf("unclosed environment quote on line %d", line)
			}
			value = value[1 : len(value)-1]
		}
		values[key] = value
	}
	if err = scanner.Err(); err != nil {
		return fmt.Errorf("cannot parse environment file") // Never echo its contents.
	}
	for key, value := range values {
		if _, set := os.LookupEnv(key); !set {
			if err = os.Setenv(key, value); err != nil {
				return fmt.Errorf("cannot set environment key %s", key)
			}
		}
	}
	return nil
}

// Resolve existing ancestors too, so a not-yet-created home beneath a symlink
// cannot accidentally put runtime databases/credentials in a source checkout.
func resolvedPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(absolute)
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return resolved, nil
		}
		if !os.IsNotExist(err) || filepath.Dir(absolute) == absolute {
			return "", err
		}
		suffix = append(suffix, filepath.Base(absolute))
		absolute = filepath.Dir(absolute)
	}
}

func OutsideSource(root, home string) error {
	r, err := resolvedPath(root)
	if err != nil {
		return err
	}
	h, err := resolvedPath(home)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(r, h)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("AIH_HOME must be outside the source checkout (including symlinks)")
	}
	return nil
}

// ParseLocal reads only configuration, without Git/network operations.
func ParseLocal(root string) (Effective, error) {
	files := map[string]string{}
	for _, name := range []string{"project.yaml", "policies.yaml", "harness.lock"} {
		data, err := os.ReadFile(filepath.Join(root, ".aih", name))
		if err != nil {
			return Effective{}, err
		}
		files[".aih/"+name] = string(bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf}))
	}
	return Parse(files)
}
