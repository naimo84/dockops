package dotenv

import (
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want map[string]string
	}{
		{"pairs", "A=1\nB=two words\n", map[string]string{"A": "1", "B": "two words"}},
		{"comments and blanks", "# lead\nA=1\n\n   \n#trail\n", map[string]string{"A": "1"}},
		{"line without equals ignored", "A=1\njust text\n", map[string]string{"A": "1"}},
		{"empty key ignored", "A=1\n= VALUE\n", map[string]string{"A": "1"}},
		{"whitespace around key and value", "  A  =  1  \n", map[string]string{"A": "1"}},
		{"crlf line endings", "A=1\r\nB=2\r\n", map[string]string{"A": "1", "B": "2"}},
		{"first equals wins", "URL=http://x?a=b\n", map[string]string{"URL": "http://x?a=b"}},
		{"no shell expansion", "A=$HOME `id` {{var}}\n", map[string]string{"A": "$HOME `id` {{var}}"}},
		{"empty value", "A=\n", map[string]string{"A": ""}},
		{"empty input", "", map[string]string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Parse([]byte(tc.in)); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Parse(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestFormatDeterministic(t *testing.T) {
	env := map[string]string{"B": "2", "A": "1", "C": "3"}
	first := Format(env)
	// A differently built but equal map must format identically.
	other := map[string]string{"C": "3", "A": "1", "B": "2"}
	if second := Format(other); string(first) != string(second) {
		t.Errorf("Format not deterministic: %q vs %q", first, second)
	}
	if want := "A=1\nB=2\nC=3\n"; string(first) != want {
		t.Errorf("Format() = %q, want %q", first, want)
	}
}

func TestRoundTrip(t *testing.T) {
	env := map[string]string{"ALPHA": "one", "BETA": "two words", "GAMMA": ""}
	if got := Parse(Format(env)); !reflect.DeepEqual(got, env) {
		t.Errorf("Parse(Format(env)) = %v, want %v", got, env)
	}
}

func TestLiteral(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		// Values compose reads verbatim stay as they are, so ordinary
		// generated files keep their KEY=value form.
		{"plain", "pg-secret", "pg-secret"},
		{"inner space", "two words", "two words"},
		{"url", "https://x.example/p?a=b&c=d", "https://x.example/p?a=b&c=d"},
		{"inner tab", "a\tb", "a\tb"},
		{"empty", "", ""},
		{"utf-8", "päss→wört", "päss→wört"},
		// Everything compose would read differently gets quoted.
		{"dollar", "pa$word", `"pa$$word"`},
		{"braced reference", "${HOME}", `"$${HOME}"`},
		{"inline comment", "abc #1", `"abc #1"`},
		{"hash", "a#b", `"a#b"`},
		{"double quotes", `say "hi"`, `"say \"hi\""`},
		{"leading single quote", "'x'", `"'x'"`},
		{"backtick", "a`b", "\"a`b\""},
		{"backslash", `back\slash`, `"back\\slash"`},
		{"newline", "line1\nline2", `"line1\nline2"`},
		{"carriage return", "a\rb", `"a\rb"`},
		{"leading space", " x", `" x"`},
		{"trailing tab", "x\t", "\"x\t\""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Literal(tc.in); got != tc.want {
				t.Errorf("Literal(%q) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}
