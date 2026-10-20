package config

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/mail"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// EnrollmentConfig turns on self-service API keys: a person opens a join
// link, enters a work email in one of the join key's domains, confirms by
// email, and is shown a key of their own (or replaces it later). Keys stay in
// this config file like every other key.
type EnrollmentConfig struct {
	// PublicURL is the origin the emailed links point at, e.g.
	// https://llm.example.com. Required to send links: the request's Host
	// header is never used, so a forged Host can't redirect a link.
	PublicURL string          `yaml:"public_url,omitempty"`
	Paused    bool            `yaml:"paused,omitempty"` // stops every join link
	SMTP      SMTPConfig      `yaml:"smtp,omitempty"`
	Downloads DownloadsConfig `yaml:"downloads,omitempty"`
	JoinKeys  []JoinKeyConfig `yaml:"join_keys,omitempty"`
}

// SMTPConfig is the mail relay enrollment links are sent through.
type SMTPConfig struct {
	Host     string `yaml:"host,omitempty"`
	Port     int    `yaml:"port,omitempty"` // default 587
	Username string `yaml:"username,omitempty"`
	// Password is the SMTP password written inline. PasswordFile (an absolute
	// path, read at send time) is preferred so the secret stays out of YAML.
	Password     string `yaml:"password,omitempty"`
	PasswordFile string `yaml:"password_file,omitempty"`
	From         string `yaml:"from,omitempty"`
	ReplyTo      string `yaml:"reply_to,omitempty"`
	// STARTTLS upgrades the connection before authenticating (default true).
	STARTTLS *bool `yaml:"starttls,omitempty"`
}

// DownloadsConfig holds the code-link links shown with a new key. Empty
// fields use the defaults below.
type DownloadsConfig struct {
	MacOS      string `yaml:"macos,omitempty"`
	Windows    string `yaml:"windows,omitempty"`
	Quickstart string `yaml:"quickstart,omitempty"`
}

const (
	DefaultDownloadMacOS      = "https://codelink.eidonix.com/download/macos"
	DefaultDownloadWindows    = "https://codelink.eidonix.com/download/windows"
	DefaultDownloadQuickstart = "https://codelink.eidonix.com/quickstart"
	DefaultSMTPPort           = 587
)

// JoinKeyConfig is one join link: who may use it (email domains) and what
// the keys it issues may reach.
type JoinKeyConfig struct {
	ID      string   `yaml:"id"`               // stable id, recorded on each issued key
	Name    string   `yaml:"name"`             // shown on the join page, e.g. the company
	Key     string   `yaml:"key"`              // random secret in the link /join/<key>
	Domains []string `yaml:"domains"`          // lowercase email domains, exact match
	Models  []string `yaml:"models,omitempty"` // issued keys' allowed models; empty = all
	Paused  bool     `yaml:"paused,omitempty"`
	// Public also accepts the join key's domains on the bare /join page
	// (code-link's "Get a key" link), so people need no link of their own.
	// A domain may belong to at most one public join key.
	Public bool `yaml:"public,omitempty"`
}

// StartTLS reports whether the relay connection is upgraded with STARTTLS.
func (s SMTPConfig) StartTLS() bool { return s.STARTTLS == nil || *s.STARTTLS }

// EffectivePort returns the SMTP port with the default applied.
func (s SMTPConfig) EffectivePort() int {
	if s.Port == 0 {
		return DefaultSMTPPort
	}
	return s.Port
}

// PasswordSet reports whether a password is configured either way.
func (s SMTPConfig) PasswordSet() bool { return s.Password != "" || s.PasswordFile != "" }

// ResolvePassword returns the SMTP password: the contents of PasswordFile
// (trimmed) when set, otherwise the inline Password.
func (s SMTPConfig) ResolvePassword() (string, error) {
	if s.PasswordFile == "" {
		return s.Password, nil
	}
	data, err := os.ReadFile(s.PasswordFile)
	if err != nil {
		return "", fmt.Errorf("reading smtp password_file: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

// Effective returns the download links with defaults applied.
func (d DownloadsConfig) Effective() DownloadsConfig {
	if d.MacOS == "" {
		d.MacOS = DefaultDownloadMacOS
	}
	if d.Windows == "" {
		d.Windows = DefaultDownloadWindows
	}
	if d.Quickstart == "" {
		d.Quickstart = DefaultDownloadQuickstart
	}
	return d
}

// Missing lists what enrollment still needs before join links can work.
// Empty means ready.
func (e *EnrollmentConfig) Missing() []string {
	var out []string
	if e.PublicURL == "" {
		out = append(out, "public URL")
	}
	if e.SMTP.Host == "" {
		out = append(out, "SMTP host")
	}
	if e.SMTP.From == "" {
		out = append(out, "From address")
	}
	return out
}

// Ready reports whether join links can send confirmation email.
func (e *EnrollmentConfig) Ready() bool { return len(e.Missing()) == 0 }

// FindJoinKeyByKey returns the join key whose link secret is key, or nil.
func (e *EnrollmentConfig) FindJoinKeyByKey(key string) *JoinKeyConfig {
	if key == "" {
		return nil
	}
	for i := range e.JoinKeys {
		if constantTimeStringEqual(e.JoinKeys[i].Key, key) {
			return &e.JoinKeys[i]
		}
	}
	return nil
}

func constantTimeStringEqual(a, b string) bool {
	ah, bh := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ah[:], bh[:]) == 1
}

// PublicJoinKeyFor returns the active public join key whose domains include
// the normalized email's domain, or nil.
func (e *EnrollmentConfig) PublicJoinKeyFor(email string) *JoinKeyConfig {
	for i := range e.JoinKeys {
		j := &e.JoinKeys[i]
		if j.Public && !j.Paused && j.AllowsEmail(email) {
			return j
		}
	}
	return nil
}

// HasPublicJoinKey reports whether any public join key is active.
func (e *EnrollmentConfig) HasPublicJoinKey() bool {
	for _, j := range e.JoinKeys {
		if j.Public && !j.Paused {
			return true
		}
	}
	return false
}

// FindJoinKey returns the join key with the given id, or nil.
func (e *EnrollmentConfig) FindJoinKey(id string) *JoinKeyConfig {
	for i := range e.JoinKeys {
		if e.JoinKeys[i].ID == id {
			return &e.JoinKeys[i]
		}
	}
	return nil
}

// AllowsEmail reports whether a normalized email's domain is one of the join
// key's domains (exact match).
func (j *JoinKeyConfig) AllowsEmail(email string) bool {
	at := strings.LastIndexByte(email, '@')
	if at < 0 {
		return false
	}
	domain := email[at+1:]
	for _, d := range j.Domains {
		if d == domain {
			return true
		}
	}
	return false
}

// FindKeyByEmail returns the enrolled key for email, or nil.
func FindKeyByEmail(cfg *Config, email string) *KeyConfig {
	if email == "" {
		return nil
	}
	for i := range cfg.Keys {
		if cfg.Keys[i].Email == email {
			return &cfg.Keys[i]
		}
	}
	return nil
}

var (
	emailLocalRe = regexp.MustCompile(`^[a-z0-9!#$%&'*+/=?^_{|}~-]+(\.[a-z0-9!#$%&'*+/=?^_{|}~-]+)*$`)
	domainRe     = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
	joinKeyIDRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
)

// NormalizeEmail lowercases and checks a plain address (user@example.com):
// ASCII only, no display name, quoting, comments or IP-literal domains.
func NormalizeEmail(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	bad := fmt.Errorf("invalid email address")
	if s == "" || len(s) > 254 {
		return "", bad
	}
	at := strings.LastIndexByte(s, '@')
	if at <= 0 || at > 64 || at == len(s)-1 {
		return "", bad
	}
	local, domain := s[:at], s[at+1:]
	if !emailLocalRe.MatchString(local) || !ValidEmailDomain(domain) {
		return "", bad
	}
	if a, err := mail.ParseAddress(s); err != nil || a.Address != s || a.Name != "" {
		return "", bad
	}
	return s, nil
}

// ValidEmailDomain reports whether d is a lowercase DNS name with a dot and
// an alphabetic top-level label.
func ValidEmailDomain(d string) bool {
	return len(d) <= 253 && domainRe.MatchString(d)
}

// normalizeDomain accepts "Example.com" or "@example.com" and returns the
// lowercase domain.
func normalizeDomain(d string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(d), "@"))
}

func validateHTTPURL(label, raw string, originOnly bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s is invalid: %w", label, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s must use http or https, got %q", label, raw)
	}
	if u.Host == "" {
		return fmt.Errorf("%s missing host", label)
	}
	if u.User != nil {
		return fmt.Errorf("%s must not contain credentials", label)
	}
	if originOnly && ((u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "") {
		return fmt.Errorf("%s must be an origin like https://llm.example.com (no path)", label)
	}
	return nil
}

// validateEnrollment checks the enrollment block and the enrollment fields
// of keys. names holds every model name a key may reference.
func validateEnrollment(cfg *Config, names map[string]bool) error {
	e := &cfg.Enrollment
	if e.PublicURL != "" {
		if err := validateHTTPURL("enrollment.public_url", e.PublicURL, true); err != nil {
			return err
		}
	}
	s := e.SMTP
	if s.Port < 0 || s.Port > 65535 {
		return fmt.Errorf("enrollment.smtp.port must be between 1 and 65535")
	}
	if strings.ContainsAny(s.Host, "/: ") {
		return fmt.Errorf("enrollment.smtp.host must be a host name (put the port in port)")
	}
	for label, addr := range map[string]string{"from": s.From, "reply_to": s.ReplyTo} {
		if addr == "" {
			continue
		}
		if _, err := mail.ParseAddress(addr); err != nil {
			return fmt.Errorf("enrollment.smtp.%s is not a valid address: %w", label, err)
		}
	}
	if s.PasswordFile != "" && !strings.HasPrefix(s.PasswordFile, "/") {
		return fmt.Errorf("enrollment.smtp.password_file must be an absolute path")
	}
	for label, raw := range map[string]string{
		"enrollment.downloads.macos":      e.Downloads.MacOS,
		"enrollment.downloads.windows":    e.Downloads.Windows,
		"enrollment.downloads.quickstart": e.Downloads.Quickstart,
	} {
		if raw == "" {
			continue
		}
		if err := validateHTTPURL(label, raw, false); err != nil {
			return err
		}
	}

	ids := make(map[string]bool)
	secrets := make(map[string]bool)
	publicDomains := make(map[string]string) // domain -> public join key id
	for i, j := range e.JoinKeys {
		label := fmt.Sprintf("enrollment.join_keys[%d]", i)
		if !joinKeyIDRe.MatchString(j.ID) {
			return fmt.Errorf("%s: id must be 1-40 lowercase letters, digits or dashes", label)
		}
		if ids[j.ID] {
			return fmt.Errorf("%s: duplicate id %q", label, j.ID)
		}
		ids[j.ID] = true
		if strings.TrimSpace(j.Name) == "" {
			return fmt.Errorf("%s: name is required", label)
		}
		if len(j.Name) > 64 {
			return fmt.Errorf("%s: name too long (max 64 chars)", label)
		}
		if len(j.Key) < 16 {
			return fmt.Errorf("%s: key must be at least 16 characters", label)
		}
		if secrets[j.Key] {
			return fmt.Errorf("%s: duplicate key", label)
		}
		secrets[j.Key] = true
		if len(j.Domains) == 0 {
			return fmt.Errorf("%s: at least one email domain is required", label)
		}
		for _, d := range j.Domains {
			if !ValidEmailDomain(d) {
				return fmt.Errorf("%s: invalid domain %q (lowercase, like example.com)", label, d)
			}
			if j.Public {
				if other, ok := publicDomains[d]; ok {
					return fmt.Errorf("%s: domain %q is already in public join key %q", label, d, other)
				}
				publicDomains[d] = j.ID
			}
		}
		for _, m := range j.Models {
			if !names[m] {
				return fmt.Errorf("%s references unknown model %q", label, m)
			}
		}
	}

	emails := make(map[string]bool)
	for _, k := range cfg.Keys {
		if k.Email == "" {
			if k.JoinKey != "" {
				return fmt.Errorf("key %q has join_key but no email", k.Name)
			}
			continue
		}
		norm, err := NormalizeEmail(k.Email)
		if err != nil || norm != k.Email {
			return fmt.Errorf("key %q has an invalid email (must be a lowercase address)", k.Name)
		}
		if emails[k.Email] {
			return fmt.Errorf("duplicate key email %q", k.Email)
		}
		emails[k.Email] = true
	}
	return nil
}

// ─── Mutations ───────────────────────────────────────────────────────────────

// EnrollmentSettings are the admin-editable enrollment fields other than join
// keys. SMTP.Password left empty keeps the stored password.
type EnrollmentSettings struct {
	PublicURL string
	Paused    bool
	SMTP      SMTPConfig
	Downloads DownloadsConfig
}

// enrollmentNode returns the enrollment mapping in root, creating it.
func enrollmentNode(root *yaml.Node) (*yaml.Node, error) {
	n := findMappingValue(root, "enrollment")
	if n == nil {
		n = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		setMappingValue(root, "enrollment", n)
	}
	if n.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("enrollment section is not a mapping")
	}
	return n, nil
}

func setOrDelete(m *yaml.Node, key, val string) {
	if val == "" {
		deleteMappingValue(m, key)
	} else {
		setMappingValue(m, key, stringNode(val))
	}
}

// UpdateEnrollmentSettings saves the public URL, pause switch, SMTP relay and
// download links. An empty SMTP password keeps the stored one unless the
// username is cleared too.
func (cs *ConfigStore) UpdateEnrollmentSettings(s EnrollmentSettings) error {
	s.PublicURL = strings.TrimRight(strings.TrimSpace(s.PublicURL), "/")
	// A blank password keeps the stored one, unless the username is cleared
	// or a password file now holds the secret.
	cur := cs.Get().Enrollment.SMTP
	if s.SMTP.Password == "" && s.SMTP.Username != "" && strings.TrimSpace(s.SMTP.PasswordFile) == "" {
		s.SMTP.Password = cur.Password
	}
	return cs.mutateYAML(func(root *yaml.Node) error {
		e, err := enrollmentNode(root)
		if err != nil {
			return err
		}
		setOrDelete(e, "public_url", s.PublicURL)
		if s.Paused {
			setMappingValue(e, "paused", boolNode(true))
		} else {
			deleteMappingValue(e, "paused")
		}

		smtp := findMappingValue(e, "smtp")
		if smtp == nil || smtp.Kind != yaml.MappingNode {
			smtp = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			setMappingValue(e, "smtp", smtp)
		}
		setOrDelete(smtp, "host", strings.TrimSpace(s.SMTP.Host))
		if s.SMTP.Port == 0 || s.SMTP.Port == DefaultSMTPPort {
			deleteMappingValue(smtp, "port")
		} else {
			setMappingValue(smtp, "port", intNode(s.SMTP.Port))
		}
		setOrDelete(smtp, "username", strings.TrimSpace(s.SMTP.Username))
		setOrDelete(smtp, "password", s.SMTP.Password)
		setOrDelete(smtp, "password_file", strings.TrimSpace(s.SMTP.PasswordFile))
		setOrDelete(smtp, "from", strings.TrimSpace(s.SMTP.From))
		setOrDelete(smtp, "reply_to", strings.TrimSpace(s.SMTP.ReplyTo))
		if s.SMTP.StartTLS() {
			deleteMappingValue(smtp, "starttls")
		} else {
			setMappingValue(smtp, "starttls", boolNode(false))
		}
		if len(smtp.Content) == 0 {
			deleteMappingValue(e, "smtp")
		}

		dl := findMappingValue(e, "downloads")
		if dl == nil || dl.Kind != yaml.MappingNode {
			dl = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			setMappingValue(e, "downloads", dl)
		}
		for _, f := range []struct{ key, val, def string }{
			{"macos", s.Downloads.MacOS, DefaultDownloadMacOS},
			{"windows", s.Downloads.Windows, DefaultDownloadWindows},
			{"quickstart", s.Downloads.Quickstart, DefaultDownloadQuickstart},
		} {
			val := strings.TrimSpace(f.val)
			if val == f.def {
				val = "" // the default needs no line in the file
			}
			setOrDelete(dl, f.key, val)
		}
		if len(dl.Content) == 0 {
			deleteMappingValue(e, "downloads")
		}
		if len(e.Content) == 0 {
			deleteMappingValue(root, "enrollment")
		}
		return nil
	})
}

// GenerateJoinSecret returns a random 32-hex-character join link secret.
func GenerateJoinSecret() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// joinKeyID derives an unused id from name: lowercase letters and digits
// joined by dashes, with -2, -3… on a clash.
func joinKeyID(name string, taken func(string) bool) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(r)
			dash = false
		} else {
			dash = true
		}
		if b.Len() >= 32 {
			break
		}
	}
	base := strings.Trim(b.String(), "-")
	if base == "" {
		base = "join"
	}
	id := base
	for n := 2; taken(id); n++ {
		id = base + "-" + strconv.Itoa(n)
	}
	return id
}

func cleanDomains(domains []string) ([]string, error) {
	var out []string
	seen := make(map[string]bool)
	for _, d := range domains {
		d = normalizeDomain(d)
		if d == "" || seen[d] {
			continue
		}
		if !ValidEmailDomain(d) {
			return nil, fmt.Errorf("invalid domain %q", d)
		}
		seen[d] = true
		out = append(out, d)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("at least one email domain is required")
	}
	return out, nil
}

func joinKeyNode(j JoinKeyConfig) *yaml.Node {
	n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	n.Content = append(n.Content,
		stringNode("id"), stringNode(j.ID),
		stringNode("name"), stringNode(j.Name),
		stringNode("key"), stringNode(j.Key),
		stringNode("domains"), flowStringSeqNode(j.Domains),
	)
	if len(j.Models) > 0 {
		n.Content = append(n.Content, stringNode("models"), flowStringSeqNode(j.Models))
	}
	if j.Paused {
		n.Content = append(n.Content, stringNode("paused"), boolNode(true))
	}
	if j.Public {
		n.Content = append(n.Content, stringNode("public"), boolNode(true))
	}
	return n
}

// AddJoinKey creates a join link for name, limited to domains, issuing keys
// limited to models (empty = all); public also accepts the domains on the
// bare /join page. Returns the new join key.
func (cs *ConfigStore) AddJoinKey(name string, domains, models []string, public bool) (JoinKeyConfig, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return JoinKeyConfig{}, fmt.Errorf("name is required")
	}
	if len(name) > 64 {
		return JoinKeyConfig{}, fmt.Errorf("name too long (max 64 chars)")
	}
	domains, err := cleanDomains(domains)
	if err != nil {
		return JoinKeyConfig{}, err
	}
	secret, err := GenerateJoinSecret()
	if err != nil {
		return JoinKeyConfig{}, fmt.Errorf("generating join key: %w", err)
	}
	cur := cs.Get()
	j := JoinKeyConfig{
		ID:      joinKeyID(name, func(id string) bool { return cur.Enrollment.FindJoinKey(id) != nil }),
		Name:    name,
		Key:     secret,
		Domains: domains,
		Models:  models,
		Public:  public,
	}
	err = cs.mutateYAML(func(root *yaml.Node) error {
		e, err := enrollmentNode(root)
		if err != nil {
			return err
		}
		seq := findMappingValue(e, "join_keys")
		if seq == nil {
			seq = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
			setMappingValue(e, "join_keys", seq)
		}
		if seq.Kind != yaml.SequenceNode {
			return fmt.Errorf("enrollment.join_keys is not a sequence")
		}
		seq.Content = append(seq.Content, joinKeyNode(j))
		return nil
	})
	if err != nil {
		return JoinKeyConfig{}, err
	}
	return j, nil
}

// mutateJoinKey applies fn to the join_keys entry with id; fn returning
// remove=true deletes the entry.
func (cs *ConfigStore) mutateJoinKey(id string, fn func(entry *yaml.Node) (remove bool)) error {
	if cs.Get().Enrollment.FindJoinKey(id) == nil {
		return fmt.Errorf("join key not found")
	}
	return cs.mutateYAML(func(root *yaml.Node) error {
		seq := findMappingValue(findMappingValue(root, "enrollment"), "join_keys")
		if seq == nil || seq.Kind != yaml.SequenceNode {
			return fmt.Errorf("join key not found in config file")
		}
		for i, entry := range seq.Content {
			v := findMappingValue(entry, "id")
			if v == nil || v.Value != id {
				continue
			}
			if fn(entry) {
				seq.Content = append(seq.Content[:i], seq.Content[i+1:]...)
				if len(seq.Content) == 0 {
					deleteMappingValue(findMappingValue(root, "enrollment"), "join_keys")
				}
			}
			return nil
		}
		return fmt.Errorf("join key not found in config file")
	})
}

// UpdateJoinKey changes a join key's name, domains, models and public flag;
// its link stays the same.
func (cs *ConfigStore) UpdateJoinKey(id, name string, domains, models []string, public bool) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("name is required")
	}
	if len(name) > 64 {
		return fmt.Errorf("name too long (max 64 chars)")
	}
	domains, err := cleanDomains(domains)
	if err != nil {
		return err
	}
	return cs.mutateJoinKey(id, func(entry *yaml.Node) bool {
		setMappingValue(entry, "name", stringNode(name))
		setMappingValue(entry, "domains", flowStringSeqNode(domains))
		if len(models) == 0 {
			deleteMappingValue(entry, "models")
		} else {
			setMappingValue(entry, "models", flowStringSeqNode(models))
		}
		if public {
			setMappingValue(entry, "public", boolNode(true))
		} else {
			deleteMappingValue(entry, "public")
		}
		return false
	})
}

// SetJoinKeyPaused pauses or resumes one join link.
func (cs *ConfigStore) SetJoinKeyPaused(id string, paused bool) error {
	return cs.mutateJoinKey(id, func(entry *yaml.Node) bool {
		if paused {
			setMappingValue(entry, "paused", boolNode(true))
		} else {
			deleteMappingValue(entry, "paused")
		}
		return false
	})
}

// DeleteJoinKey removes a join link. Keys it issued keep working; revoke
// them on the API Keys page.
func (cs *ConfigStore) DeleteJoinKey(id string) error {
	return cs.mutateJoinKey(id, func(*yaml.Node) bool { return true })
}

// EnrollKey issues the key for email: a new key (Name = email, allowed
// models from the join key) when the address has none, otherwise a new
// secret for its existing key (name and models kept; the old key stops
// working). The check and the write happen under one config lock.
func (cs *ConfigStore) EnrollKey(email, joinKeyID string, models []string) (key string, replaced bool, err error) {
	norm, err := NormalizeEmail(email)
	if err != nil || norm != email {
		return "", false, fmt.Errorf("invalid email address")
	}
	key, err = GenerateKey()
	if err != nil {
		return "", false, fmt.Errorf("generating key: %w", err)
	}
	err = cs.mutateYAML(func(root *yaml.Node) error {
		keysNode := findMappingValue(root, "keys")
		if keysNode == nil {
			keysNode = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
			setMappingValue(root, "keys", keysNode)
		}
		if keysNode.Kind != yaml.SequenceNode {
			return fmt.Errorf("keys section is not a sequence")
		}
		for _, entry := range keysNode.Content {
			if v := findMappingValue(entry, "email"); v != nil && v.Value == email {
				setMappingValue(entry, "key", stringNode(key))
				replaced = true
				return nil
			}
		}
		entry := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		entry.Content = append(entry.Content,
			stringNode("key"), stringNode(key),
			stringNode("name"), stringNode(email),
			stringNode("email"), stringNode(email),
			stringNode("join_key"), stringNode(joinKeyID),
		)
		if len(models) > 0 {
			entry.Content = append(entry.Content, stringNode("models"), flowStringSeqNode(models))
		}
		keysNode.Content = append(keysNode.Content, entry)
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return key, replaced, nil
}
