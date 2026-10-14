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
