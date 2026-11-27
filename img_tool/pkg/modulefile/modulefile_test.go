package modulefile

import (
	"reflect"
	"strings"
	"testing"
)

const sample = `module(name = "example", version = "1.0")

# A comment, then a call spanning lines.
bazel_dep(name = "rules_img", version = "0.3.0", repo_name = "img")

DIGEST = "sha256:" + "abc"
REGISTRIES = ["mirror.gcr.io"]
REGISTRIES += ["index.docker.io"]

images = use_extension("@img//img:extensions.bzl", "images", dev_dependency = True)
images.pull(
    name = "nginx",
    digest = DIGEST,
    registries = REGISTRIES,
    repository = 'library/nginx',
    tag = """1.27""",
    platforms = [p for p in ["linux/amd64"]],
    raw = r"a\"b\n",
    escaped = "a\tb\x41é",
)
use_repo(images, "rules_img_images.bzl", nginx = "img_abc")  # trailing comment
a, b = "x", "y"
x = 1 if True else 2
y = not -1 in [1] and {"k": "v"}["k"] != None
z = foo(*args, **kwargs)[0:1].bar
`

func TestParse(t *testing.T) {
	file, err := Parse("MODULE.bazel", []byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	env := Env{}
	var calls []*Call
	for _, stmt := range file.Stmts {
		switch stmt := stmt.(type) {
		case *Assign:
			stmt.Bind(env)
			if call, ok := stmt.Value.(*Call); ok {
				calls = append(calls, call)
			}
		case *ExprStmt:
			if call, ok := stmt.X.(*Call); ok {
				calls = append(calls, call)
			}
		}
	}
	var lines []int
	for _, call := range calls {
		lines = append(lines, call.Start().Line)
	}
	if want := []int{1, 4, 10, 11, 21}; !reflect.DeepEqual(lines, want) {
		t.Errorf("call lines = %v, want %v", lines, want)
	}

	pull := calls[3]
	if dot, ok := pull.Fn.(*Dot); !ok || dot.Name != "pull" || dot.X.(*Ident).Name != "images" {
		t.Fatalf("calls[3] is not images.pull: %#v", pull.Fn)
	}
	for attr, want := range map[string]Value{
		"name":       "nginx",
		"digest":     "sha256:abc",
		"registries": []Value{"mirror.gcr.io", "index.docker.io"},
		"repository": "library/nginx",
		"tag":        "1.27",
		"platforms":  Unknown{},
		"raw":        `a\"b\n`,
		"escaped":    "a\tbAé",
	} {
		if got := Eval(pull.Keyword(attr), env); !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %#v, want %#v", attr, got, want)
		}
	}
	if IsLiteral(pull.Keyword("digest")) || !IsLiteral(pull.Keyword("name")) {
		t.Errorf("IsLiteral(digest) = true or IsLiteral(name) = false")
	}
	if got := Eval(calls[2].Keyword("dev_dependency"), env); got != true {
		t.Errorf("dev_dependency = %#v", got)
	}
	if got := env["a"]; got != "x" {
		t.Errorf("a = %#v", got)
	}
	if got := env["x"]; got != (Unknown{}) {
		t.Errorf("x = %#v", got)
	}
	useRepo := calls[4]
	if got := len(useRepo.Positional()); got != 2 {
		t.Errorf("use_repo has %d positional arguments", got)
	}
	if got := Eval(useRepo.Keyword("nginx"), env); got != "img_abc" {
		t.Errorf("nginx = %#v", got)
	}
}

func TestParseErrors(t *testing.T) {
	for src, want := range map[string]string{
		"def foo():\n  pass\n": `1:1: "def" statements are not allowed`,
		"foo(\n":               "unexpected end of file inside brackets",
		"foo(a b)\n":           "in arguments",
		"x = 'abc\n":           "unterminated string",
		"foo())\n":             `unbalanced ")"`,
		"foo() bar()\n":        "after statement",
	} {
		_, err := Parse("MODULE.bazel", []byte(src))
		if err == nil {
			t.Errorf("Parse(%q) succeeded", src)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Parse(%q) = %v, want it to contain %q", src, err, want)
		}
	}
}

func TestLineContinuationAndSemicolons(t *testing.T) {
	file, err := Parse("MODULE.bazel", []byte("x = \\\n  'a'; y = 'b'\n\n\nfoo(\n  x,\n)\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(file.Stmts) != 3 {
		t.Fatalf("got %d statements, want 3", len(file.Stmts))
	}
	if got := file.Stmts[2].(*ExprStmt).X.Start().Line; got != 5 {
		t.Errorf("foo starts on line %d, want 5", got)
	}
}
