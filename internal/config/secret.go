package config

import "log/slog"

// Redacted is what a non-empty Secret looks like anywhere it is printed.
const Redacted = "***"

// Secret is a string that refuses to reveal itself by accident. Formatting it
// with fmt, logging it with slog, or marshalling it to YAML or JSON all yield
// the redaction placeholder. Call Reveal where the real value is needed.
type Secret string

// Reveal returns the underlying secret value.
func (s Secret) Reveal() string { return string(s) }

func (s Secret) redacted() string {
	if s == "" {
		return ""
	}
	return Redacted
}

func (s Secret) String() string               { return s.redacted() }
func (s Secret) GoString() string             { return s.redacted() }
func (s Secret) LogValue() slog.Value         { return slog.StringValue(s.redacted()) }
func (s Secret) MarshalYAML() (any, error)    { return s.redacted(), nil }
func (s Secret) MarshalText() ([]byte, error) { return []byte(s.redacted()), nil }
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + s.redacted() + `"`), nil }
