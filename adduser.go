package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// runAddUser interactively creates a new API key and appends it to config.yaml.
// It reads the existing config, prompts for name and optional model restrictions,
// generates a secure random key, writes the updated config, and prints the result.
func runAddUser(configPath string) {
	// Load existing config.
	data, err := os.ReadFile(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading %s: %v\n", configPath, err)
		os.Exit(1)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing %s: %v\n", configPath, err)
		os.Exit(1)
	}

	reader := bufio.NewReader(os.Stdin)

	// Prompt for name.
	fmt.Print("User name: ")
	name, _ := reader.ReadString('\n')
	name = strings.TrimSpace(name)
	if name == "" {
		fmt.Fprintln(os.Stderr, "Name is required.")
		os.Exit(1)
	}

	// Show available models and prompt for restrictions.
	if len(cfg.Models) > 0 {
		fmt.Println("\nAvailable models:")
		for i, m := range cfg.Models {
			fmt.Printf("  %d. %s\n", i+1, m.Name)
		}
		fmt.Println("\nRestrict to specific models? Enter comma-separated numbers,")
		fmt.Print("or press Enter for access to all models: ")
	} else {
		fmt.Print("Model restrictions (comma-separated names, or Enter for all): ")
	}

	modelsInput, _ := reader.ReadString('\n')
	modelsInput = strings.TrimSpace(modelsInput)

	var models []string
	if modelsInput != "" {
		parts := strings.Split(modelsInput, ",")
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			// Try as a number first (index into model list).
			var idx int
			if _, err := fmt.Sscanf(p, "%d", &idx); err == nil && idx >= 1 && idx <= len(cfg.Models) {
				models = append(models, cfg.Models[idx-1].Name)
			} else {
				// Otherwise treat as a model name.
				models = append(models, p)
			}
		}
	}

	// Generate a secure random API key.
	keyBytes := make([]byte, 24)
	if _, err := rand.Read(keyBytes); err != nil {
		fmt.Fprintf(os.Stderr, "Error generating key: %v\n", err)
		os.Exit(1)
	}
	key := "dy-" + hex.EncodeToString(keyBytes)

	// Append the new key to config.
	cfg.Keys = append(cfg.Keys, KeyConfig{
		Key:    key,
		Name:   name,
		Models: models,
	})

	// Write the updated config.
	out, err := yaml.Marshal(&cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error marshaling config: %v\n", err)
		os.Exit(1)
	}

	if err := os.WriteFile(configPath, out, 0600); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing %s: %v\n", configPath, err)
		os.Exit(1)
	}

	// Print result.
	fmt.Println("\n" + strings.Repeat("─", 50))
	fmt.Printf("User added: %s\n", name)
	fmt.Printf("API Key:    %s\n", key)
	if len(models) == 0 {
		fmt.Println("Access:     All models")
	} else {
		fmt.Printf("Access:     %s\n", strings.Join(models, ", "))
	}
	fmt.Println(strings.Repeat("─", 50))
	fmt.Println("\nThe config file has been updated. If the proxy is running,")
	fmt.Println("the new key will be active within seconds (auto-reload).")
}
