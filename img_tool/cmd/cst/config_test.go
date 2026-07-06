package cst

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseConfigYAML(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "test.yaml")
	content := `schemaVersion: "2.0.0"
metadataTest:
  envVars:
    - key: PATH
      value: /usr/bin
  cmd: ["/bin/sh", "-c"]
  workdir: /app
fileExistenceTests:
  - name: hello
    path: /hello.txt
    shouldExist: true
    permissions: "-rw-r--r--"
    uid: 0
commandTests:
  - name: run
    command: echo
`
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := parseConfig(p)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if st.SchemaVersion != "2.0.0" {
		t.Errorf("schemaVersion = %q", st.SchemaVersion)
	}
	env := st.MetadataTest.mergedEnv()
	if len(env) != 1 || env[0].Key != "PATH" || env[0].Value != "/usr/bin" {
		t.Errorf("envVars = %+v", env)
	}
	if st.MetadataTest.Cmd == nil || len(*st.MetadataTest.Cmd) != 2 {
		t.Errorf("cmd = %+v", st.MetadataTest.Cmd)
	}
	if len(st.FileExistenceTests) != 1 {
		t.Fatalf("fileExistenceTests = %+v", st.FileExistenceTests)
	}
	fe := st.FileExistenceTests[0]
	if fe.Path != "/hello.txt" || !fe.ShouldExist || fe.Permissions != "-rw-r--r--" {
		t.Errorf("fileExistenceTest = %+v", fe)
	}
	if fe.Uid == nil || *fe.Uid != 0 {
		t.Errorf("uid = %v (want pointer to 0)", fe.Uid)
	}
	if len(st.CommandTests) != 1 {
		t.Errorf("commandTests = %+v", st.CommandTests)
	}
}

func TestParseConfigJSON(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "test.json")
	content := `{"schemaVersion":"2.0.0","metadataTest":{"user":"nobody","entrypoint":[]}}`
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := parseConfig(p)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if st.MetadataTest.User != "nobody" {
		t.Errorf("user = %q", st.MetadataTest.User)
	}
	// An explicit empty entrypoint must be a non-nil, empty slice ("assert unset").
	if st.MetadataTest.Entrypoint == nil {
		t.Errorf("entrypoint pointer is nil; expected explicit empty list")
	} else if len(*st.MetadataTest.Entrypoint) != 0 {
		t.Errorf("entrypoint = %+v", *st.MetadataTest.Entrypoint)
	}
}

func TestUidOmittedIsNil(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "test.yaml")
	if err := os.WriteFile(p, []byte("fileExistenceTests:\n  - path: /x\n    shouldExist: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := parseConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.FileExistenceTests[0].Uid != nil {
		t.Errorf("omitted uid should be nil, got %v", st.FileExistenceTests[0].Uid)
	}
}
