package kvfile

import (
	"errors"
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  map[string][]string
	}{
		{
			name:  "json object string values",
			input: `{"org.opencontainers.image.version": "1.0.0", "author": "team@example.com"}`,
			want: map[string][]string{
				"org.opencontainers.image.version": {"1.0.0"},
				"author":                           {"team@example.com"},
			},
		},
		{
			name:  "json object array values",
			input: `{"key": ["value1", "value2"]}`,
			want:  map[string][]string{"key": {"value1", "value2"}},
		},
		{
			name:  "json object mixed string and array values",
			input: `{"single": "v", "multi": ["a", "b"]}`,
			want:  map[string][]string{"single": {"v"}, "multi": {"a", "b"}},
		},
		{
			name:  "json array of key=value",
			input: `["key=value", "other=thing"]`,
			want:  map[string][]string{"key": {"value"}, "other": {"thing"}},
		},
		{
			name:  "json array repeated key",
			input: `["key=a", "key=b"]`,
			want:  map[string][]string{"key": {"a", "b"}},
		},
		{
			name:  "newline delimited text",
			input: "key=value\nother=thing\n",
			want:  map[string][]string{"key": {"value"}, "other": {"thing"}},
		},
		{
			name:  "text skips blanks and comments",
			input: "# a comment\nkey=value\n\n# another\nother=thing\n",
			want:  map[string][]string{"key": {"value"}, "other": {"thing"}},
		},
		{
			name:  "text trims whitespace around key and value",
			input: "  key   =   value  \n",
			want:  map[string][]string{"key": {"value"}},
		},
		{
			name:  "value containing equals via text",
			input: "key=a=b=c\n",
			want:  map[string][]string{"key": {"a=b=c"}},
		},
		{
			name:  "json preserves spaces and equals verbatim",
			input: `{"key": "  spaced = value  "}`,
			want:  map[string][]string{"key": {"  spaced = value  "}},
		},
		{
			name:  "json preserves newlines in value",
			input: `{"key": "line1\nline2"}`,
			want:  map[string][]string{"key": {"line1\nline2"}},
		},
		{
			name:  "empty value",
			input: "key=\n",
			want:  map[string][]string{"key": {""}},
		},
		{
			name:  "empty input",
			input: "",
			want:  map[string][]string{},
		},
		{
			name:  "whitespace only input",
			input: "   \n  \n",
			want:  map[string][]string{},
		},
		{
			name:  "empty json object",
			input: `{}`,
			want:  map[string][]string{},
		},
		{
			name:  "leading whitespace before json",
			input: "  \n  {\"key\": \"value\"}",
			want:  map[string][]string{"key": {"value"}},
		},
		{
			name:  "utf8 bom before json object",
			input: "\xEF\xBB\xBF{\"key\": \"value\"}",
			want:  map[string][]string{"key": {"value"}},
		},
		{
			name:  "utf8 bom before text",
			input: "\xEF\xBB\xBFkey=value\n",
			want:  map[string][]string{"key": {"value"}},
		},
		{
			name:  "json array skips empty and whitespace elements",
			input: "[\n\"key=value\",\n\"\",\n\"   \"\n]",
			want:  map[string][]string{"key": {"value"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse([]byte(tc.input))
			if err != nil {
				t.Fatalf("Parse(%q) returned error: %v", tc.input, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Parse(%q) = %#v, want %#v", tc.input, got, tc.want)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantErrIs error
	}{
		{
			name:      "plain list text is not key value",
			input:     "latest\nv1.0.0\n",
			wantErrIs: ErrNotKeyValue,
		},
		{
			name:      "plain json array is not key value",
			input:     `["latest", "v1.0.0"]`,
			wantErrIs: ErrNotKeyValue,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.input))
			if err == nil {
				t.Fatalf("Parse(%q) expected error, got nil", tc.input)
			}
			if tc.wantErrIs != nil && !errors.Is(err, tc.wantErrIs) {
				t.Errorf("Parse(%q) error = %v, want errors.Is %v", tc.input, err, tc.wantErrIs)
			}
		})
	}
}

func TestParseMalformed(t *testing.T) {
	// Inputs that look like JSON (start with { or [) but are malformed should
	// report a parse error rather than silently being treated as text.
	for _, input := range []string{
		`{"key": }`,
		`{"key": "value"`,
		`["unterminated`,
		`=value`,          // empty key (text)
		"key=value\nbare", // mixed key=value and plain entries
	} {
		if _, err := Parse([]byte(input)); err == nil {
			t.Errorf("Parse(%q) expected error, got nil", input)
		}
	}
}

func TestFlatten(t *testing.T) {
	tests := []struct {
		name string
		in   map[string][]string
		want map[string]string
	}{
		{
			name: "single values",
			in:   map[string][]string{"a": {"1"}, "b": {"2"}},
			want: map[string]string{"a": "1", "b": "2"},
		},
		{
			name: "last value wins",
			in:   map[string][]string{"a": {"1", "2", "3"}},
			want: map[string]string{"a": "3"},
		},
		{
			name: "empty slice dropped",
			in:   map[string][]string{"a": {}, "b": {"2"}},
			want: map[string]string{"b": "2"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Flatten(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Flatten(%#v) = %#v, want %#v", tc.in, got, tc.want)
			}
		})
	}
}

func TestToEnv(t *testing.T) {
	in := map[string][]string{
		"PATH":     {"/usr/bin"},
		"APP_NAME": {"myapp"},
		"DUP":      {"first", "last"},
	}
	want := []string{"APP_NAME=myapp", "DUP=last", "PATH=/usr/bin"}
	got := ToEnv(in)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ToEnv(%#v) = %#v, want %#v (sorted, last value wins)", in, got, want)
	}
}

func TestParseFlexibleKinds(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantKind Kind
		wantList []string
	}{
		{name: "empty", input: "", wantKind: KindEmpty},
		{name: "key value text", input: "k=v", wantKind: KindKeyValue},
		{name: "key value json object", input: `{"k":"v"}`, wantKind: KindKeyValue},
		{name: "key value json array", input: `["k=v"]`, wantKind: KindKeyValue},
		{name: "plain list text", input: "latest\nstable", wantKind: KindList, wantList: []string{"latest", "stable"}},
		{name: "plain list json", input: `["latest", "stable"]`, wantKind: KindList, wantList: []string{"latest", "stable"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseFlexible([]byte(tc.input))
			if err != nil {
				t.Fatalf("ParseFlexible(%q) error: %v", tc.input, err)
			}
			if got.Kind != tc.wantKind {
				t.Errorf("ParseFlexible(%q) Kind = %v, want %v", tc.input, got.Kind, tc.wantKind)
			}
			if tc.wantList != nil && !reflect.DeepEqual(got.List, tc.wantList) {
				t.Errorf("ParseFlexible(%q) List = %#v, want %#v", tc.input, got.List, tc.wantList)
			}
		})
	}
}
