package config

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"gopkg.in/yaml.v3"
)

// KeyHash returns the stable client-facing identifier for an API key: the
// first 16 hex chars of sha256(key). Safe to expose to browsers — the full
// key can't be reversed from this.
func KeyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:8])
}

// MaskKey produces the display form "prefix...suffix" for an API key — first
// 7 chars plus the last 2. Short keys fall back to a generic mask.
func MaskKey(key string) string {
	if len(key) < 10 {
		return "***"
	}
	return key[:7] + "..." + key[len(key)-2:]
}

// MaskSecret is like MaskKey but uses a shorter prefix (4 chars) — appropriate
// for backend credentials that aren't the primary user-facing identifier.
func MaskSecret(v string) string {
	if len(v) < 8 {
		if v == "" {
			return ""
		}
		return "***"
	}
	return v[:4] + "…" + v[len(v)-2:]
}

// GenerateKey produces a cryptographically random API key prefixed "dy-".
// 24 bytes of entropy → 48 hex chars after the prefix.
func GenerateKey() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "dy-" + hex.EncodeToString(buf), nil
}

// mutateYAML reads the config file as a yaml.Node tree (preserving comments),
// applies mutate to the top-level mapping node, validates the result by
// round-tripping through the Config struct, writes atomically via tmp+rename,
// then reloads the in-memory Config.
func (cs *ConfigStore) mutateYAML(mutate func(root *yaml.Node) error) error {
	cs.writeMu.Lock()
	defer cs.writeMu.Unlock()

	absPath, err := filepath.Abs(cs.path)
	if err != nil {
		return fmt.Errorf("resolving config path: %w", err)
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return fmt.Errorf("reading config: %w", err)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parsing config: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return fmt.Errorf("config root is not a document node")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("config root is not a mapping")
	}

	if err := mutate(root); err != nil {
		return err
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return fmt.Errorf("encoding config: %w", err)
	}
	enc.Close()

	// Validate the serialized form by re-parsing and running the same checks
	// Load() would. Catches mutations that produce a valid YAML tree but an
	// invalid Config (e.g. duplicate model name) before touching disk.
	var validated Config
	if err := yaml.Unmarshal(buf.Bytes(), &validated); err != nil {
		return fmt.Errorf("re-parsing config: %w", err)
	}
	for i := range validated.Models {
		m := &validated.Models[i]
		if m.Timeout == 0 {
			m.Timeout = 300
		}
		if m.Model == "" {
			m.Model = m.Name
		}
		if m.Type == BackendBedrock {
			applyBedrockDefaults(m)
		}
	}
	if err := validateConfig(&validated); err != nil {
		return fmt.Errorf("resulting config is invalid: %w", err)
	}

	tmpPath := absPath + ".tmp"
	if err := os.WriteFile(tmpPath, buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("writing tmp config: %w", err)
	}
	if err := os.Rename(tmpPath, absPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("renaming config: %w", err)
	}

	// Refresh in-memory state immediately so subsequent Get() calls see the
	// new config without waiting for the fsnotify debounce. The later
	// fsnotify-triggered Load() is idempotent.
	if err := cs.Load(); err != nil {
		return fmt.Errorf("reloading after save: %w", err)
	}
	return nil
}

// ─── yaml.Node helpers ───────────────────────────────────────────────────────

func findMappingValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func setMappingValue(m *yaml.Node, key string, value *yaml.Node) {
	if m == nil || m.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = value
			return
		}
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		value,
	)
}

func deleteMappingValue(m *yaml.Node, key string) {
	if m == nil || m.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

func stringNode(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}

func intNode(v int) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(v)}
}

func floatNode(v float64) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: strconv.FormatFloat(v, 'f', -1, 64)}
}

func boolNode(v bool) *yaml.Node {
	if v {
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "true"}
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "false"}
}

func flowStringSeqNode(values []string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle}
	for _, v := range values {
		n.Content = append(n.Content, stringNode(v))
	}
	return n
}

// ─── Users (API keys) ────────────────────────────────────────────────────────

// AddKey generates a new random API key and appends it to the config. Returns
// the full key on success — callers should display it to the user exactly
// once and never persist it elsewhere.
func (cs *ConfigStore) AddKey(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	if len(name) > 64 {
		return "", fmt.Errorf("name too long (max 64 chars)")
	}

	key, err := GenerateKey()
	if err != nil {
		return "", fmt.Errorf("generating key: %w", err)
	}

	// Extremely unlikely, but check for collision.
	cur := cs.Get()
	for _, k := range cur.Keys {
		if k.Key == key {
			return "", fmt.Errorf("key collision, please retry")
		}
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
		entry := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		entry.Content = append(entry.Content,
			stringNode("key"), stringNode(key),
			stringNode("name"), stringNode(name),
		)
		keysNode.Content = append(keysNode.Content, entry)
		return nil
	})
	if err != nil {
		return "", err
	}
	return key, nil
}

// UpdateKeyModels replaces the allowed-model list for the key identified by
// keyHash. An empty slice clears the restriction (meaning "all models").
func (cs *ConfigStore) UpdateKeyModels(keyHash string, models []string) error {
	cur := cs.Get()
	fullKey := lookupKeyByHash(cur, keyHash)
	if fullKey == "" {
		return fmt.Errorf("key not found")
	}

	known := make(map[string]bool, len(cur.Models))
	for _, m := range cur.Models {
		known[m.Name] = true
	}
	for _, m := range models {
		if !known[m] {
			return fmt.Errorf("unknown model %q", m)
		}
	}

	return cs.mutateYAML(func(root *yaml.Node) error {
		return mutateKeyEntry(root, fullKey, func(entry *yaml.Node) error {
			if len(models) == 0 {
				deleteMappingValue(entry, "models")
			} else {
				setMappingValue(entry, "models", flowStringSeqNode(models))
			}
			return nil
		})
	})
}

// RenameKey sets the friendly name for the key identified by keyHash.
func (cs *ConfigStore) RenameKey(keyHash, newName string) error {
	if newName == "" {
		return fmt.Errorf("name is required")
	}
	if len(newName) > 64 {
		return fmt.Errorf("name too long (max 64 chars)")
	}
	fullKey := lookupKeyByHash(cs.Get(), keyHash)
	if fullKey == "" {
		return fmt.Errorf("key not found")
	}
	return cs.mutateYAML(func(root *yaml.Node) error {
		return mutateKeyEntry(root, fullKey, func(entry *yaml.Node) error {
			setMappingValue(entry, "name", stringNode(newName))
			return nil
		})
	})
}

// DeleteKey removes the key identified by keyHash from the config. Refuses
// to delete the last remaining key to avoid locking all API clients out.
func (cs *ConfigStore) DeleteKey(keyHash string) error {
	cur := cs.Get()
	if len(cur.Keys) <= 1 {
		return fmt.Errorf("cannot delete the last remaining key")
	}
	fullKey := lookupKeyByHash(cur, keyHash)
	if fullKey == "" {
		return fmt.Errorf("key not found")
	}
	return cs.mutateYAML(func(root *yaml.Node) error {
		keysNode := findMappingValue(root, "keys")
		if keysNode == nil || keysNode.Kind != yaml.SequenceNode {
			return fmt.Errorf("keys section not found")
		}
		for i, entry := range keysNode.Content {
			if entry.Kind != yaml.MappingNode {
				continue
			}
			keyVal := findMappingValue(entry, "key")
			if keyVal == nil || keyVal.Value != fullKey {
				continue
			}
			keysNode.Content = append(keysNode.Content[:i], keysNode.Content[i+1:]...)
			return nil
		}
		return fmt.Errorf("key not found in config file")
	})
}

func lookupKeyByHash(cfg *Config, keyHash string) string {
	for _, k := range cfg.Keys {
		if KeyHash(k.Key) == keyHash {
			return k.Key
		}
	}
	return ""
}

// mutateKeyEntry finds the key sequence entry in root whose "key" field matches
// fullKey and applies fn to it. Returns error if the entry isn't found.
func mutateKeyEntry(root *yaml.Node, fullKey string, fn func(*yaml.Node) error) error {
	keysNode := findMappingValue(root, "keys")
	if keysNode == nil || keysNode.Kind != yaml.SequenceNode {
		return fmt.Errorf("keys section not found")
	}
	for _, entry := range keysNode.Content {
		if entry.Kind != yaml.MappingNode {
			continue
		}
		keyVal := findMappingValue(entry, "key")
		if keyVal == nil || keyVal.Value != fullKey {
			continue
		}
		return fn(entry)
	}
	return fmt.Errorf("key not found in config file")
}

// ─── Models ──────────────────────────────────────────────────────────────────

// AddModel appends a new model to the config.
func (cs *ConfigStore) AddModel(m ModelConfig) error {
	if m.Name == "" {
		return fmt.Errorf("model name is required")
	}
	cur := cs.Get()
	for _, existing := range cur.Models {
		if existing.Name == m.Name {
			return fmt.Errorf("model %q already exists", m.Name)
		}
	}
	return cs.mutateYAML(func(root *yaml.Node) error {
		modelsNode := findMappingValue(root, "models")
		if modelsNode == nil {
			modelsNode = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
			setMappingValue(root, "models", modelsNode)
		}
		if modelsNode.Kind != yaml.SequenceNode {
			return fmt.Errorf("models section is not a sequence")
		}
		modelsNode.Content = append(modelsNode.Content, modelConfigNode(m))
		return nil
	})
}

// UpdateModel replaces the model identified by originalName with m. If
// m.Name != originalName, the model is renamed (new name must be unique).
func (cs *ConfigStore) UpdateModel(originalName string, m ModelConfig) error {
	if m.Name == "" {
		return fmt.Errorf("model name is required")
	}
	cur := cs.Get()
	found := false
	for _, existing := range cur.Models {
		if existing.Name == originalName {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("model %q not found", originalName)
	}
	if m.Name != originalName {
		for _, existing := range cur.Models {
			if existing.Name == m.Name {
				return fmt.Errorf("model %q already exists", m.Name)
			}
		}
	}
	return cs.mutateYAML(func(root *yaml.Node) error {
		modelsNode := findMappingValue(root, "models")
		if modelsNode == nil || modelsNode.Kind != yaml.SequenceNode {
			return fmt.Errorf("models section not found")
		}
		for i, entry := range modelsNode.Content {
			if entry.Kind != yaml.MappingNode {
				continue
			}
			nameVal := findMappingValue(entry, "name")
			if nameVal == nil || nameVal.Value != originalName {
				continue
			}
			modelsNode.Content[i] = modelConfigNode(m)
			return nil
		}
		return fmt.Errorf("model %q not found in config file", originalName)
	})
}

// DeleteModel removes the named model. If force is false and the model is
// referenced by keys or processors, the delete is refused with a descriptive
// error listing the referrers.
func (cs *ConfigStore) DeleteModel(name string, force bool) error {
	cur := cs.Get()
	if !force {
		refs := modelReferrers(cur, name)
		if len(refs) > 0 {
			return fmt.Errorf("model %q is referenced by: %v", name, refs)
		}
	}
	return cs.mutateYAML(func(root *yaml.Node) error {
		modelsNode := findMappingValue(root, "models")
		if modelsNode == nil || modelsNode.Kind != yaml.SequenceNode {
			return fmt.Errorf("models section not found")
		}
		for i, entry := range modelsNode.Content {
			if entry.Kind != yaml.MappingNode {
				continue
			}
			nameVal := findMappingValue(entry, "name")
			if nameVal == nil || nameVal.Value != name {
				continue
			}
			modelsNode.Content = append(modelsNode.Content[:i], modelsNode.Content[i+1:]...)
			// Force-delete also needs to strip references to this model from
			// key allow-lists; otherwise validateConfig will reject the save.
			if force {
				stripModelReferences(root, name)
			}
			return nil
		}
		return fmt.Errorf("model %q not found in config file", name)
	})
}

func modelReferrers(cfg *Config, name string) []string {
	var refs []string
	for _, k := range cfg.Keys {
		for _, m := range k.Models {
			if m == name {
				label := k.Name
				if label == "" {
					label = "key " + MaskKey(k.Key)
				}
				refs = append(refs, label)
				break
			}
		}
	}
	if cfg.Processors.Vision == name {
		refs = append(refs, "processors.vision")
	}
	if cfg.Processors.OCR == name {
		refs = append(refs, "processors.ocr")
	}
	for _, m := range cfg.Models {
		if m.Processors == nil {
			continue
		}
		if m.Processors.Vision == name {
			refs = append(refs, "models."+m.Name+".processors.vision")
		}
		if m.Processors.OCR == name {
			refs = append(refs, "models."+m.Name+".processors.ocr")
		}
	}
	return refs
}

// stripModelReferences removes all occurrences of modelName from key allow-lists
// and unsets processor fields that point at it. Used only when force-deleting.
func stripModelReferences(root *yaml.Node, modelName string) {
	keysNode := findMappingValue(root, "keys")
	if keysNode != nil && keysNode.Kind == yaml.SequenceNode {
		for _, entry := range keysNode.Content {
			if entry.Kind != yaml.MappingNode {
				continue
			}
			modelsVal := findMappingValue(entry, "models")
			if modelsVal == nil || modelsVal.Kind != yaml.SequenceNode {
				continue
			}
			filtered := modelsVal.Content[:0]
			for _, child := range modelsVal.Content {
				if child.Value != modelName {
					filtered = append(filtered, child)
				}
			}
			if len(filtered) == 0 {
				deleteMappingValue(entry, "models")
			} else {
				modelsVal.Content = filtered
			}
		}
	}
	procs := findMappingValue(root, "processors")
	if procs != nil && procs.Kind == yaml.MappingNode {
		if v := findMappingValue(procs, "vision"); v != nil && v.Value == modelName {
			deleteMappingValue(procs, "vision")
		}
		if v := findMappingValue(procs, "ocr"); v != nil && v.Value == modelName {
			deleteMappingValue(procs, "ocr")
		}
	}
}

// modelConfigNode renders a ModelConfig as a mapping node, omitting zero-valued
// fields so the resulting YAML stays clean.
func modelConfigNode(m ModelConfig) *yaml.Node {
	n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	add := func(key string, value *yaml.Node) {
		n.Content = append(n.Content, stringNode(key), value)
	}
	add("name", stringNode(m.Name))
	add("backend", stringNode(m.Backend))
	if m.APIKey != "" {
		add("api_key", stringNode(m.APIKey))
	}
	if m.Model != "" && m.Model != m.Name {
		add("model", stringNode(m.Model))
	}
	if m.Timeout != 0 && m.Timeout != 300 {
		add("timeout", intNode(m.Timeout))
	}
	if m.Type != "" {
		add("type", stringNode(m.Type))
	}
	if m.ResponsesMode != "" {
		add("responses_mode", stringNode(m.ResponsesMode))
	}
	if m.MessagesMode != "" {
		add("messages_mode", stringNode(m.MessagesMode))
	}
	if m.ContextWindow != 0 {
		add("context_window", intNode(m.ContextWindow))
	}
	if m.SupportsVision {
		add("supports_vision", boolNode(true))
	}
	if m.ForcePipeline {
		add("force_pipeline", boolNode(true))
	}
	if m.Region != "" {
		add("region", stringNode(m.Region))
	}
	if m.AWSAccessKey != "" {
		add("aws_access_key", stringNode(m.AWSAccessKey))
	}
	if m.AWSSecretKey != "" {
		add("aws_secret_key", stringNode(m.AWSSecretKey))
	}
	if m.AWSSessionToken != "" {
		add("aws_session_token", stringNode(m.AWSSessionToken))
	}
	if m.GuardrailID != "" {
		add("guardrail_id", stringNode(m.GuardrailID))
	}
	if m.GuardrailVersion != "" {
		add("guardrail_version", stringNode(m.GuardrailVersion))
	}
	if m.GuardrailTrace != "" {
		add("guardrail_trace", stringNode(m.GuardrailTrace))
	}
	if m.Defaults != nil && !samplingDefaultsEmpty(*m.Defaults) {
		add("defaults", samplingDefaultsNode(*m.Defaults))
	}
	if m.Processors != nil && (m.Processors.Vision != "" || m.Processors.OCR != "" || m.Processors.WebSearchKey != "") {
		add("processors", processorsNode(*m.Processors))
	}
	return n
}

func samplingDefaultsEmpty(d SamplingDefaults) bool {
	return d.Temperature == nil && d.TopP == nil && d.TopK == nil &&
		d.MaxNewTokens == nil && d.FrequencyPenalty == nil && d.PresencePenalty == nil &&
		d.ReasoningEffort == nil && len(d.Stop) == 0
}

func samplingDefaultsNode(d SamplingDefaults) *yaml.Node {
	n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	add := func(key string, value *yaml.Node) {
		n.Content = append(n.Content, stringNode(key), value)
	}
	if d.Temperature != nil {
		add("temperature", floatNode(*d.Temperature))
	}
	if d.TopP != nil {
		add("top_p", floatNode(*d.TopP))
	}
	if d.TopK != nil {
		add("top_k", intNode(*d.TopK))
	}
	if d.MaxNewTokens != nil {
		add("max_new_tokens", intNode(*d.MaxNewTokens))
	}
	if d.FrequencyPenalty != nil {
		add("frequency_penalty", floatNode(*d.FrequencyPenalty))
	}
	if d.PresencePenalty != nil {
		add("presence_penalty", floatNode(*d.PresencePenalty))
	}
	if d.ReasoningEffort != nil {
		add("reasoning_effort", stringNode(*d.ReasoningEffort))
	}
	if len(d.Stop) > 0 {
		add("stop", flowStringSeqNode(d.Stop))
	}
	return n
}

func processorsNode(p ProcessorsConfig) *yaml.Node {
	n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	add := func(key string, value *yaml.Node) {
		n.Content = append(n.Content, stringNode(key), value)
	}
	if p.Vision != "" {
		add("vision", stringNode(p.Vision))
	}
	if p.OCR != "" {
		add("ocr", stringNode(p.OCR))
	}
	if p.WebSearchKey != "" {
		add("web_search_key", stringNode(p.WebSearchKey))
	}
	return n
}
