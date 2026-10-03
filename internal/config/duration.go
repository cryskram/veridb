package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that unmarshals from either a Go duration string
// ("15s", "1m30s") or a plain integer interpreted as milliseconds.
//
//	statement_timeout: 15s
//	statement_timeout: 15000   # same thing
type Duration time.Duration

func (d Duration) Std() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	// Try an integer first: milliseconds.
	var ms int64
	if err := node.Decode(&ms); err == nil {
		*d = Duration(time.Duration(ms) * time.Millisecond)
		return nil
	}

	var raw string
	if err := node.Decode(&raw); err != nil {
		return fmt.Errorf("duration must be a string like \"15s\" or an integer of milliseconds: %s", node.Value)
	}

	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", raw, err)
	}

	*d = Duration(parsed)
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }
