package config

import (
	"fmt"
	"os"
	"regexp"
	"sort"
)

// envRef matches ${VAR} references. Only the braced form is expanded so that
// literal "$" characters inside passwords or DSNs are left untouched.
var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// ExpandEnv replaces ${VAR} references in s with their environment values.
// Unlike os.ExpandEnv it fails loudly when a referenced variable is unset,
// which turns a typo inside a DSN into a startup error instead of a confusing
// connection failure.
func ExpandEnv(s string) (string, error) {
	return ExpandEnvWith(s, os.LookupEnv)
}

// ExpandEnvWith is ExpandEnv with an injectable environment, used by tests.
func ExpandEnvWith(s string, lookup EnvLookup) (string, error) {
	var missing []string

	expanded := envRef.ReplaceAllStringFunc(s, func(match string) string {
		name := envRef.FindStringSubmatch(match)[1]

		value, ok := lookup(name)
		if !ok {
			missing = append(missing, name)
			return match
		}

		return value
	})

	if len(missing) > 0 {
		sort.Strings(missing)
		return "", fmt.Errorf("undefined environment variable(s): %s", joinComma(missing))
	}

	return expanded, nil
}

func joinComma(items []string) string {
	out := ""
	for i, item := range items {
		if i > 0 {
			out += ", "
		}
		out += item
	}
	return out
}
