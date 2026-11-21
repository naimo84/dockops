package compose

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/local/dockops/internal/dotenv"
)

// Lookup resolves one variable for compose-style interpolation; ok is false
// when the variable is unset. A nil Lookup knows no variables.
type Lookup func(name string) (value string, ok bool)

// EnvLookup returns the Lookup docker compose itself would interpolate a
// deploy with: the process environment first (compose lets the shell win over
// --env-file), then the values of envFile. A missing envFile is not an error
// — compose then sees the process environment only — but an unreadable one
// is.
func EnvLookup(envFile string) (Lookup, error) {
	values := map[string]string{}
	if envFile != "" {
		raw, err := os.ReadFile(envFile)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read env file %s: %w", envFile, err)
		}
		for k, v := range dotenv.Parse(raw) {
			values[k] = unquoteDotenv(v)
		}
	}
	return func(name string) (string, bool) {
		if v, ok := os.LookupEnv(name); ok {
			return v, true
		}
		v, ok := values[name]
		return v, ok
	}, nil
}

// unquoteDotenv reverses the quoting compose's dotenv parser applies to a
// value (see dotenv.Literal): double quotes process \\ \" \n \r and $$,
// single quotes are literal, an unquoted value ends at an inline " #".
func unquoteDotenv(v string) string {
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		inner := v[1 : len(v)-1]
		var b strings.Builder
		for i := 0; i < len(inner); i++ {
			c := inner[i]
			if c == '\\' && i+1 < len(inner) {
				i++
				switch inner[i] {
				case 'n':
					b.WriteByte('\n')
				case 'r':
					b.WriteByte('\r')
				default:
					b.WriteByte(inner[i])
				}
				continue
			}
			if c == '$' && i+1 < len(inner) && inner[i+1] == '$' {
				i++
			}
			b.WriteByte(c)
		}
		return b.String()
	}
	if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
		return v[1 : len(v)-1]
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return v
}

// interpolate expands a compose value the way docker compose does before it
// parses the value: $VAR, ${VAR} and the operators ${VAR:-default},
// ${VAR-default}, ${VAR:?message}, ${VAR?message}, ${VAR:+alt} and
// ${VAR+alt}; operands are interpolated themselves (nested defaults), and $$
// is a literal $. An unset variable without operator expands to "" (compose
// warns and does the same).
func interpolate(s string, lookup Lookup) (string, error) {
	if lookup == nil {
		lookup = func(string) (string, bool) { return "", false }
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '$' || i+1 == len(s) {
			b.WriteByte(c)
			continue
		}
		next := s[i+1]
		switch {
		case next == '$':
			b.WriteByte('$')
			i++
		case next == '{':
			end, err := closingBrace(s, i+2)
			if err != nil {
				return "", err
			}
			value, err := expandBraced(s[i+2:end], lookup)
			if err != nil {
				return "", err
			}
			b.WriteString(value)
			i = end
		case isNameStart(next):
			j := i + 1
			for j < len(s) && isNameChar(s[j]) {
				j++
			}
			value, _ := lookup(s[i+1 : j])
			b.WriteString(value)
			i = j - 1
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), nil
}

// closingBrace returns the index of the "}" that closes the ${ opened just
// before start, skipping nested ${...} in operands.
func closingBrace(s string, start int) (int, error) {
	depth := 1
	for i := start; i < len(s); i++ {
		switch {
		case s[i] == '$' && i+1 < len(s) && s[i+1] == '{':
			depth++
			i++
		case s[i] == '}':
			depth--
			if depth == 0 {
				return i, nil
			}
		}
	}
	return 0, fmt.Errorf("unclosed ${ in %q", s)
}

// expandBraced resolves the inside of one ${...}: a name, optionally followed
// by an operator and its operand.
func expandBraced(expr string, lookup Lookup) (string, error) {
	n := 0
	for n < len(expr) && isNameChar(expr[n]) {
		n++
	}
	name, rest := expr[:n], expr[n:]
	if name == "" || !isNameStart(name[0]) {
		return "", fmt.Errorf("invalid variable name in ${%s}", expr)
	}
	value, set := lookup(name)
	if rest == "" {
		return value, nil
	}
	var op string
	for _, candidate := range []string{":-", ":?", ":+", "-", "?", "+"} {
		if strings.HasPrefix(rest, candidate) {
			op = candidate
			break
		}
	}
	if op == "" {
		return "", fmt.Errorf("unsupported expression ${%s}", expr)
	}
	operand := rest[len(op):]
	// "unset" for the plain operators, "unset or empty" for the colon forms.
	missing := !set
	if strings.HasPrefix(op, ":") {
		missing = !set || value == ""
	}
	switch strings.TrimPrefix(op, ":") {
	case "-":
		if missing {
			return interpolate(operand, lookup)
		}
		return value, nil
	case "?":
		if missing {
			message, err := interpolate(operand, lookup)
			if err != nil {
				return "", err
			}
			return "", fmt.Errorf("required variable %s is missing a value: %s", name, message)
		}
		return value, nil
	default: // "+"
		if missing {
			return "", nil
		}
		return interpolate(operand, lookup)
	}
}

func isNameStart(c byte) bool {
	return c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

func isNameChar(c byte) bool {
	return isNameStart(c) || (c >= '0' && c <= '9')
}
