package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

const legacyConfigBackupSuffix = ".pre-backend-lists"

// migrateLegacyBackendConfig performs the one-way schema migration from
// top-level named pools and single model backends to model-owned backend
// lists. The active Config type deliberately has no pools or pool reference:
// this small YAML migration is the complete backwards-compatibility boundary.
func migrateLegacyBackendConfig(data []byte) ([]byte, bool, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, false, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, false, fmt.Errorf("config root is not a mapping")
	}
	root := doc.Content[0]

	pools := make(map[string]*yaml.Node)
	if poolsNode := findMappingValue(root, "pools"); poolsNode != nil {
		if poolsNode.Kind != yaml.SequenceNode {
			return nil, false, fmt.Errorf("legacy pools section is not a sequence")
		}
		for _, entry := range poolsNode.Content {
			if entry.Kind != yaml.MappingNode {
				return nil, false, fmt.Errorf("legacy pool entry is not a mapping")
			}
			name := findMappingValue(entry, "name")
			backends := findMappingValue(entry, "backends")
			if name == nil || name.Value == "" || backends == nil {
				return nil, false, fmt.Errorf("legacy pool entry requires name and backends")
			}
			if _, exists := pools[name.Value]; exists {
				return nil, false, fmt.Errorf("duplicate legacy pool %q", name.Value)
			}
			pools[name.Value] = backends
		}
	}

	changed := findMappingValue(root, "pools") != nil
	models := findMappingValue(root, "models")
	if models != nil {
		if models.Kind != yaml.SequenceNode {
			return nil, false, fmt.Errorf("models section is not a sequence")
		}
		for _, model := range models.Content {
			if model.Kind != yaml.MappingNode {
				return nil, false, fmt.Errorf("model entry is not a mapping")
			}
			modelName := "<unnamed>"
			if n := findMappingValue(model, "name"); n != nil && n.Value != "" {
				modelName = n.Value
			}

			backends := findMappingValue(model, "backends")
			pool := findMappingValue(model, "pool")
			backend := findMappingValue(model, "backend")
			modelKey := findMappingValue(model, "api_key")
			modelType := findMappingValue(model, "type")
			region := findMappingValue(model, "region")

			sources := 0
			if backends != nil {
				sources++
			}
			if pool != nil && pool.Value != "" {
				sources++
			}
			if backend != nil && backend.Value != "" {
				sources++
			}
			if sources > 1 {
				return nil, false, fmt.Errorf("model %q has multiple legacy backend sources", modelName)
			}

			switch {
			case pool != nil && pool.Value != "":
				poolBackends, ok := pools[pool.Value]
				if !ok {
					return nil, false, fmt.Errorf("model %q references unknown legacy pool %q", modelName, pool.Value)
				}
				backends = cloneYAMLNode(poolBackends)
				setMappingValue(model, "backends", backends)
				changed = true
			case backend != nil && backend.Value != "":
				entry := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
				setMappingValue(entry, "url", cloneYAMLNode(backend))
				if modelKey != nil && modelKey.Value != "" {
					setMappingValue(entry, "api_key", cloneYAMLNode(modelKey))
				}
				backends = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{entry}}
				setMappingValue(model, "backends", backends)
				changed = true
			case sources == 0 && modelType != nil && modelType.Value == BackendBedrock && region != nil && region.Value != "":
				entry := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
				setMappingValue(entry, "url", stringNode(fmt.Sprintf("https://bedrock-runtime.%s.amazonaws.com", region.Value)))
				if modelKey != nil && modelKey.Value != "" {
					setMappingValue(entry, "api_key", cloneYAMLNode(modelKey))
				}
				backends = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{entry}}
				setMappingValue(model, "backends", backends)
				changed = true
			}

			// A legacy model-level key was the default for every member of a
			// pool. Copy it into list entries that do not already override it.
			if modelKey != nil && modelKey.Value != "" && backends != nil && backends.Kind == yaml.SequenceNode {
				for _, entry := range backends.Content {
					if entry.Kind == yaml.MappingNode && findMappingValue(entry, "api_key") == nil {
						setMappingValue(entry, "api_key", cloneYAMLNode(modelKey))
					}
				}
				changed = true
			}

			if pool != nil || backend != nil || modelKey != nil {
				deleteMappingValue(model, "pool")
				deleteMappingValue(model, "backend")
				deleteMappingValue(model, "api_key")
				changed = true
			}
		}
	}

	if !changed {
		return data, false, nil
	}
	deleteMappingValue(root, "pools")

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, false, err
	}
	if err := enc.Close(); err != nil {
		return nil, false, err
	}
	return buf.Bytes(), true, nil
}

func cloneYAMLNode(n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	clone := *n
	clone.Content = make([]*yaml.Node, len(n.Content))
	for i, child := range n.Content {
		clone.Content[i] = cloneYAMLNode(child)
	}
	return &clone
}

func persistMigratedConfig(path string, migrated []byte) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	original, err := os.ReadFile(absPath)
	if err != nil {
		return err
	}
	backupPath := absPath + legacyConfigBackupSuffix
	backup, err := os.OpenFile(backupPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		if _, writeErr := backup.Write(original); writeErr != nil {
			backup.Close()
			return writeErr
		}
		if closeErr := backup.Close(); closeErr != nil {
			return closeErr
		}
	} else if !errors.Is(err, fs.ErrExist) {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(absPath), ".go-llm-config-migrate-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(migrated); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, absPath)
}
