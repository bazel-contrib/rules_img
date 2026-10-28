package aws

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSharedFiles(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config")
	creds := filepath.Join(dir, "credentials")
	os.WriteFile(cfg, []byte(`
# comment
[default]
region = us-west-2 ; trailing
[profile dev]
role_arn = arn:aws:iam::1:role/x
source_profile=base
s3 =
  max_concurrent_requests = 5
[sso-session corp]
sso_region = eu-west-1
[services foo]
x = y
`), 0o600)
	os.WriteFile(creds, []byte("[base]\naws_access_key_id=AK\naws_secret_access_key = SK\n[default]\nregion=ignored-but-wins\n"), 0o600)
	env := map[string]string{"AWS_CONFIG_FILE": cfg, "AWS_SHARED_CREDENTIALS_FILE": creds}
	f, err := loadSharedFiles(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if got := f.profiles["default"]["region"]; got != "ignored-but-wins" {
		t.Errorf("default region = %q", got)
	}
	if got := f.profiles["dev"]["source_profile"]; got != "base" {
		t.Errorf("dev source_profile = %q", got)
	}
	if _, ok := f.profiles["dev"]["max_concurrent_requests"]; ok {
		t.Error("sub-property leaked into profile")
	}
	if f.profiles["base"]["aws_secret_access_key"] != "SK" {
		t.Errorf("base = %v", f.profiles["base"])
	}
	if f.ssoSessions["corp"]["sso_region"] != "eu-west-1" {
		t.Errorf("sso-session = %v", f.ssoSessions)
	}
	if _, ok := f.profiles["services foo"]; ok {
		t.Error("services section not ignored")
	}
}
