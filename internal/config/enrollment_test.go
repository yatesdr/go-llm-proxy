package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const enrollFixture = baseFixture + `
# ── Enrollment ────────────────────────────────────────
enrollment:
  public_url: https://llm.example.com
  smtp:
    host: smtp.example.com
    username: sender@example.com
    password: inline-secret   # kept when the admin form is blank
    from: "LLM <no-reply@example.com>"
  join_keys:
    - id: acme
      name: Acme
      key: 0123456789abcdef0123456789abcdef # gitleaks:allow
      domains: [acme.example]
      models: [model-a]
`

func TestEnrollmentLoadsAndDefaults(t *testing.T) {
	cs := loadStore(t, writeFixture(t, enrollFixture))
	e := cs.Get().Enrollment
	if !e.Ready() {
		t.Fatalf("expected ready, missing %v", e.Missing())
	}
	if e.SMTP.EffectivePort() != 587 || !e.SMTP.StartTLS() {
		t.Errorf("defaults: port %d starttls %v", e.SMTP.EffectivePort(), e.SMTP.StartTLS())
	}
	dl := e.Downloads.Effective()
	if dl.MacOS != DefaultDownloadMacOS || dl.Windows != DefaultDownloadWindows || dl.Quickstart != DefaultDownloadQuickstart {
		t.Errorf("download defaults: %+v", dl)
	}
	jk := e.FindJoinKeyByKey("0123456789abcdef0123456789abcdef")
	if jk == nil || jk.ID != "acme" {
		t.Fatalf("join key lookup: %+v", jk)
	}
	if e.FindJoinKeyByKey("wrong") != nil || e.FindJoinKeyByKey("") != nil {
		t.Error("lookup with a wrong key matched")
	}
	if !jk.AllowsEmail("pat@acme.example") || jk.AllowsEmail("pat@sub.acme.example") || jk.AllowsEmail("pat@acme.example.evil") {
		t.Error("domain match must be exact")
	}
}

func TestEnrollmentValidation(t *testing.T) {
	cases := map[string]struct{ from, to, want string }{
		"uppercase domain":     {"domains: [acme.example]", "domains: [ACME.example]", "invalid domain"},
		"no domains":           {"domains: [acme.example]", "domains: []", "at least one email domain"},
		"short key":            {"key: 0123456789abcdef0123456789abcdef", "key: short", "at least 16"}, // gitleaks:allow
		"bad id":               {"id: acme", "id: Acme Co", "id must be"},
		"unknown model":        {"models: [model-a]\n", "models: [nope]\n", "unknown model"},
		"public url with path": {"public_url: https://llm.example.com", "public_url: https://llm.example.com/x", "no path"},
		"public url scheme":    {"public_url: https://llm.example.com", "public_url: ftp://llm.example.com", "http or https"},
		"bad from":             {`from: "LLM <no-reply@example.com>"`, "from: not an address", "smtp.from"},
		"relative pw file":     {"password: inline-secret", "password_file: secrets/pw", "absolute path"},
		"host with port":       {"host: smtp.example.com", "host: smtp.example.com:587", "host name"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(enrollFixture, c.from) {
				t.Fatalf("fixture lacks %q", c.from)
			}
			path := writeFixture(t, strings.Replace(enrollFixture, c.from, c.to, 1))
			_, err := NewConfigStore(path)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("expected error containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestEnrolledKeyValidation(t *testing.T) {
	keyBlock := "  - key: sk-existing-two\n    name: second\n"
	cases := map[string]struct{ add, want string }{
		"uppercase email": {"    email: Pat@acme.example\n", "invalid email"},
		"join_key alone":  {"    join_key: acme\n", "no email"},
		"duplicate email": {"    email: pat@acme.example\n  - key: sk-third\n    name: third\n    email: pat@acme.example\n", "duplicate key email"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeFixture(t, strings.Replace(enrollFixture, keyBlock, keyBlock+c.add, 1))
			_, err := NewConfigStore(path)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("expected error containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestNormalizeEmail(t *testing.T) {
	good := map[string]string{
		" Pat.Lee@Acme.Example ": "pat.lee@acme.example",
		"a+tag@x.example.com":    "a+tag@x.example.com",
	}
	for in, want := range good {
		got, err := NormalizeEmail(in)
		if err != nil || got != want {
			t.Errorf("NormalizeEmail(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "pat", "pat@", "@acme.example", "pat@acme", "Pat <pat@acme.example>",
		`"pat lee"@acme.example`, "pat..lee@acme.example", "pat@[127.0.0.1]", "pat@acme..example",
		"pat@-acme.example", "pàt@acme.example", "pat@acme.example\r\nBcc: x@y.example", "a@b@acme.example",
		strings.Repeat("a", 65) + "@acme.example"} {
		if got, err := NormalizeEmail(in); err == nil {
			t.Errorf("NormalizeEmail(%q) accepted as %q", in, got)
		}
	}
}

func TestEnrollKeyCreateThenRotate(t *testing.T) {
	path := writeFixture(t, enrollFixture)
	cs := loadStore(t, path)

	key1, replaced, err := cs.EnrollKey("pat@acme.example", "acme", []string{"model-a"})
	if err != nil || replaced {
		t.Fatalf("EnrollKey: %v replaced=%v", err, replaced)
	}
	k := FindKeyByEmail(cs.Get(), "pat@acme.example")
	if k == nil || k.Key != key1 || k.Name != "pat@acme.example" || k.JoinKey != "acme" || len(k.Models) != 1 {
		t.Fatalf("issued key: %+v", k)
	}

	key2, replaced, err := cs.EnrollKey("pat@acme.example", "acme", []string{"model-a"})
	if err != nil || !replaced || key2 == key1 {
		t.Fatalf("rotate: %v replaced=%v same=%v", err, replaced, key2 == key1)
	}
	cfg := cs.Get()
	n := 0
	for _, k := range cfg.Keys {
		if k.Key == key1 {
			t.Error("old key still present")
		}
		if k.Email == "pat@acme.example" {
			n++
		}
	}
	if n != 1 || len(cfg.Keys) != 3 {
		t.Fatalf("expected one key for the address and 3 keys total, got %d and %d", n, len(cfg.Keys))
	}

	data, _ := os.ReadFile(path)
	for _, want := range []string{"# test config", "# ── API Keys", "# ── Enrollment", "# kept when the admin form is blank",
		"email: pat@acme.example", "join_key: acme"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("saved config lost %q:\n%s", want, data)
		}
	}

	if _, _, err := cs.EnrollKey("Pat@Acme.example", "acme", nil); err == nil {
		t.Error("un-normalized email accepted")
	}
}

func TestJoinKeyMutations(t *testing.T) {
	path := writeFixture(t, enrollFixture)
	cs := loadStore(t, path)

	j, err := cs.AddJoinKey("Acme", []string{" @Acme.Example ", "acme.example", "other.example"}, nil, false)
	if err != nil {
		t.Fatalf("AddJoinKey: %v", err)
	}
	if j.ID != "acme-2" || len(j.Key) != 32 || strings.Join(j.Domains, ",") != "acme.example,other.example" {
		t.Fatalf("join key: %+v", j)
	}
	if got := cs.Get().Enrollment.FindJoinKeyByKey(j.Key); got == nil || got.ID != "acme-2" {
		t.Fatal("new join key not loaded")
	}
	if _, err := cs.AddJoinKey("Bad", nil, nil, false); err == nil {
		t.Error("join key without domains accepted")
	}
	if _, err := cs.AddJoinKey("Bad", []string{"not a domain"}, nil, false); err == nil {
		t.Error("invalid domain accepted")
	}
	if _, err := cs.AddJoinKey("Bad", []string{"bad.example"}, []string{"nope"}, false); err == nil {
		t.Error("unknown model accepted")
	}

	if err := cs.UpdateJoinKey("acme-2", "Acme West", []string{"west.example"}, []string{"model-b"}, true); err != nil {
		t.Fatalf("UpdateJoinKey: %v", err)
	}
	got := cs.Get().Enrollment.FindJoinKey("acme-2")
	if got.Name != "Acme West" || got.Domains[0] != "west.example" || got.Models[0] != "model-b" || got.Key != j.Key || !got.Public {
		t.Fatalf("updated: %+v", got)
	}
	if err := cs.SetJoinKeyPaused("acme-2", true); err != nil || !cs.Get().Enrollment.FindJoinKey("acme-2").Paused {
		t.Fatalf("pause: %v", err)
	}
	if err := cs.SetJoinKeyPaused("acme-2", false); err != nil || cs.Get().Enrollment.FindJoinKey("acme-2").Paused {
		t.Fatalf("resume: %v", err)
	}
	if err := cs.DeleteJoinKey("acme-2"); err != nil || cs.Get().Enrollment.FindJoinKey("acme-2") != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := cs.DeleteJoinKey("acme-2"); err == nil {
		t.Error("deleting a missing join key succeeded")
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "# ── Enrollment") || !strings.Contains(string(data), "id: acme") {
		t.Errorf("config lost content:\n%s", data)
	}
}

func TestUpdateEnrollmentSettingsKeepsSecrets(t *testing.T) {
	path := writeFixture(t, enrollFixture)
	cs := loadStore(t, path)
	off := false
	s := EnrollmentSettings{
		PublicURL: "https://llm.example.com/",
		SMTP: SMTPConfig{Host: "smtp.example.com", Port: 2525, Username: "sender@example.com",
			From: "no-reply@example.com", ReplyTo: "support@example.com", STARTTLS: &off},
		Downloads: DownloadsConfig{MacOS: DefaultDownloadMacOS, Windows: "https://dl.example.com/win"},
	}
	if err := cs.UpdateEnrollmentSettings(s); err != nil {
		t.Fatalf("save: %v", err)
	}
	e := cs.Get().Enrollment
	if e.SMTP.Password != "inline-secret" {
		t.Errorf("blank password should keep the stored one, got %q", e.SMTP.Password)
	}
	if e.PublicURL != "https://llm.example.com" || e.SMTP.Port != 2525 || e.SMTP.StartTLS() || e.SMTP.ReplyTo != "support@example.com" {
		t.Errorf("saved: %+v", e)
	}
	if e.Downloads.MacOS != "" || e.Downloads.Windows != "https://dl.example.com/win" {
		t.Errorf("downloads: %+v (a default should not be written)", e.Downloads)
	}
	if len(e.JoinKeys) != 1 {
		t.Error("join keys lost")
	}

	// Switching to a password file drops the inline password.
	pwFile := filepath.Join(t.TempDir(), "smtp_password")
	os.WriteFile(pwFile, []byte("  file-secret\n"), 0o600)
	s.SMTP.PasswordFile = pwFile
	if err := cs.UpdateEnrollmentSettings(s); err != nil {
		t.Fatalf("save: %v", err)
	}
	e = cs.Get().Enrollment
	if e.SMTP.Password != "" || !e.SMTP.PasswordSet() {
		t.Errorf("inline password should be gone and file set: %+v", e.SMTP)
	}
	if pw, err := e.SMTP.ResolvePassword(); err != nil || pw != "file-secret" {
		t.Errorf("ResolvePassword = %q, %v", pw, err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "inline-secret") || strings.Contains(string(data), "file-secret") {
		t.Error("a password was written to the config")
	}
}

func TestModelChangesFollowJoinKeys(t *testing.T) {
	path := writeFixture(t, strings.Replace(enrollFixture, "  - key: sk-existing-one\n    name: first\n    models: [model-a]\n",
		"  - key: sk-existing-one\n    name: first\n", 1))
	cs := loadStore(t, path)

	if err := cs.DeleteModel("model-a", false); err == nil || !strings.Contains(err.Error(), "join key Acme") {
		t.Fatalf("expected join key referrer, got %v", err)
	}
	m := *FindModel(cs.Get(), "model-a")
	m.Name = "model-a2"
	if err := cs.UpdateModel("model-a", m); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if got := cs.Get().Enrollment.FindJoinKey("acme").Models; len(got) != 1 || got[0] != "model-a2" {
		t.Fatalf("join key models after rename: %v", got)
	}
	if err := cs.DeleteModel("model-a2", true); err != nil {
		t.Fatalf("force delete: %v", err)
	}
	jk := cs.Get().Enrollment.FindJoinKey("acme")
	if len(jk.Models) != 0 || !jk.Paused {
		t.Fatalf("emptied join key should be paused, got %+v", jk)
	}
}

func TestPublicJoinKeys(t *testing.T) {
	path := writeFixture(t, strings.Replace(enrollFixture, "      models: [model-a]\n", "      models: [model-a]\n      public: true\n", 1))
	cs := loadStore(t, path)
	e := cs.Get().Enrollment
	if !e.HasPublicJoinKey() {
		t.Fatal("expected an active public join key")
	}
	if jk := e.PublicJoinKeyFor("pat@acme.example"); jk == nil || jk.ID != "acme" {
		t.Fatalf("PublicJoinKeyFor: %+v", jk)
	}
	if e.PublicJoinKeyFor("pat@other.example") != nil {
		t.Error("matched a domain no public join key has")
	}
	// A second public join key may not claim the same domain.
	if _, err := cs.AddJoinKey("Acme again", []string{"acme.example"}, nil, true); err == nil || !strings.Contains(err.Error(), "already in public join key") {
		t.Fatalf("expected a domain clash, got %v", err)
	}
	// ...but a private one may.
	if _, err := cs.AddJoinKey("Acme private", []string{"acme.example"}, nil, false); err != nil {
		t.Fatalf("private join key with a shared domain: %v", err)
	}
	if err := cs.SetJoinKeyPaused("acme", true); err != nil {
		t.Fatal(err)
	}
	e = cs.Get().Enrollment
	if e.HasPublicJoinKey() || e.PublicJoinKeyFor("pat@acme.example") != nil {
		t.Error("a paused public join key still matched")
	}
}
