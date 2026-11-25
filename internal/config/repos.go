package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// ReposConfig represents the repos section of config.yaml
type ReposConfig struct {
	Primary    string   `yaml:"primary,omitempty"`
	Additional []string `yaml:"additional,omitempty,flow"`
}

// configFile represents the structure for reading/writing config.yaml
// We use yaml.Node to preserve comments and formatting
type configFile struct {
	root yaml.Node
}

// FindConfigYAMLPath finds the config.yaml file in .beads directory
// Walks up from CWD to find .beads/config.yaml
func FindConfigYAMLPath() (string, error) {
	configPath, err := findProjectConfigYaml()
	if err != nil {
		return "", fmt.Errorf("no .beads/config.yaml found in current directory or parents")
	}
	return configPath, nil
}

// GetReposFromYAML reads the repos configuration from config.yaml
// Returns an empty ReposConfig if repos section doesn't exist
func GetReposFromYAML(configPath string) (*ReposConfig, error) {
	data, err := readConfigShared(configPath) // shared config lock: never a half-written file
	if err != nil {
		if os.IsNotExist(err) {
			return &ReposConfig{}, nil
		}
		return nil, fmt.Errorf("failed to read config.yaml: %w", err)
	}
	return reposFromYAML(data)
}

// reposFromYAML parses the repos section out of config.yaml's bytes.
func reposFromYAML(data []byte) (*ReposConfig, error) {
	// Parse into a generic map to extract repos section
	var cfg map[string]interface{}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config.yaml: %w", err)
	}

	repos := &ReposConfig{}
	if reposRaw, ok := cfg["repos"]; ok && reposRaw != nil {
		reposMap, ok := reposRaw.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("repos section is not a map")
		}

		if primary, ok := reposMap["primary"].(string); ok {
			repos.Primary = primary
		}

		if additional, ok := reposMap["additional"]; ok && additional != nil {
			switch v := additional.(type) {
			case []interface{}:
				for _, item := range v {
					if str, ok := item.(string); ok {
						repos.Additional = append(repos.Additional, str)
					}
				}
			}
		}
	}

	return repos, nil
}

// SetReposInYAML writes the repos configuration to config.yaml
// It preserves other config sections and comments where possible
func SetReposInYAML(configPath string, repos *ReposConfig) error {
	// No current section is parsed: a whole replacement overwrites any repos
	// value, even one that is not a map, as it always did.
	return updateReposInYAML(configPath, nil, repos)
}

// updateReposInYAML writes config.yaml's repos section in one locked rewrite.
// With change set, it parses the current section and writes what change derives
// from it, so a concurrent AddRepo or RemoveRepo is never lost; else it writes
// repos.
func updateReposInYAML(configPath string, change func(current *ReposConfig) (*ReposConfig, error), repos *ReposConfig) error {
	// Read existing config or create new, under the config write lock.
	err := RewriteConfigFile(configPath, 0600, func(data []byte, _ bool) ([]byte, error) {
		next := repos
		if change != nil {
			current, err := reposFromYAML(data)
			if err != nil {
				return nil, &reposReadError{err: err}
			}
			if next, err = change(current); err != nil {
				return nil, err
			}
		}
		return reposYAML(data, next)
	})
	if err != nil {
		return labelRewriteErr(err, "config.yaml")
	}

	// Reload viper config so changes take effect immediately. The write lock
	// is released by now; the reload parses the bytes read under the file's
	// shared lock, so it never loads a file another bd is halfway through
	// writing.
	if v != nil && v.ConfigFileUsed() != "" {
		if err := loadViperConfigLocked(v, v.ConfigFileUsed(), false); err != nil {
			// Not fatal - config is on disk, will be picked up on next command
			_ = err // Best effort: viper reload failure is non-fatal since config was already written to disk
		}
	}

	return nil
}

// reposYAML returns config.yaml's bytes data with its repos section set to repos.
func reposYAML(data []byte, repos *ReposConfig) ([]byte, error) {
	// Parse existing config into yaml.Node to preserve structure
	var root yaml.Node
	if len(data) > 0 {
		if err := yaml.Unmarshal(data, &root); err != nil {
			return nil, fmt.Errorf("failed to parse config.yaml: %w", err)
		}
	}

	// Handle empty or comment-only files by creating a valid document structure
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		root = yaml.Node{
			Kind: yaml.DocumentNode,
			Content: []*yaml.Node{
				{Kind: yaml.MappingNode},
			},
		}
	}

	// Get the mapping node (first content of document)
	mapping := root.Content[0]
	if mapping.Kind != yaml.MappingNode {
		// If the document content isn't a mapping, replace it with one
		root.Content[0] = &yaml.Node{Kind: yaml.MappingNode}
		mapping = root.Content[0]
	}

	// Find or create repos section
	reposIndex := -1
	for i := 0; i < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == "repos" {
			reposIndex = i
			break
		}
	}

	// Build the repos node
	reposNode := buildReposNode(repos)

	if reposIndex >= 0 {
		// Update existing repos section
		if reposNode == nil {
			// Remove repos section entirely if empty
			mapping.Content = append(mapping.Content[:reposIndex], mapping.Content[reposIndex+2:]...)
		} else {
			mapping.Content[reposIndex+1] = reposNode
		}
	} else if reposNode != nil {
		// Add new repos section at the end
		mapping.Content = append(mapping.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "repos"},
			reposNode,
		)
	}

	// Marshal back to YAML
	var buf strings.Builder
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(&root); err != nil {
		return nil, fmt.Errorf("failed to encode config.yaml: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("failed to close encoder: %w", err)
	}

	return []byte(buf.String()), nil
}

// buildReposNode creates a yaml.Node for the repos configuration
// Returns nil if repos is empty (no primary and no additional)
func buildReposNode(repos *ReposConfig) *yaml.Node {
	if repos == nil || (repos.Primary == "" && len(repos.Additional) == 0) {
		return nil
	}

	node := &yaml.Node{Kind: yaml.MappingNode}

	if repos.Primary != "" {
		node.Content = append(node.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "primary"},
			&yaml.Node{Kind: yaml.ScalarNode, Value: repos.Primary, Style: yaml.DoubleQuotedStyle},
		)
	}

	if len(repos.Additional) > 0 {
		additionalNode := &yaml.Node{Kind: yaml.SequenceNode}
		for _, path := range repos.Additional {
			additionalNode.Content = append(additionalNode.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Value: path, Style: yaml.DoubleQuotedStyle},
			)
		}
		node.Content = append(node.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "additional"},
			additionalNode,
		)
	}

	return node
}

// reposReadError marks a failure to parse the current repos section inside
// updateReposInYAML, which AddRepo and RemoveRepo report as they always did.
type reposReadError struct{ err error }

func (e *reposReadError) Error() string { return e.err.Error() }
func (e *reposReadError) Unwrap() error { return e.err }

// reposChangeErr restores AddRepo/RemoveRepo's historic wording for a repos
// section that cannot be read ("failed to get repos config: ...").
func reposChangeErr(err error) error {
	var rr *reposReadError
	if errors.As(err, &rr) {
		return fmt.Errorf("failed to get repos config: %w", rr.err)
	}
	var rw *rewriteError
	if errors.As(err, &rw) && rw.op == "read" {
		return fmt.Errorf("failed to get repos config: %w", labelRewriteErr(rw, "config.yaml"))
	}
	return err
}

// AddRepo adds a repository to the repos.additional list in config.yaml
// If primary is not set, it defaults to "."
func AddRepo(configPath, repoPath string) error {
	err := updateReposInYAML(configPath, func(repos *ReposConfig) (*ReposConfig, error) {
		// Set primary to "." if not already set (standard multi-repo convention)
		if repos.Primary == "" {
			repos.Primary = "."
		}

		// Check if repo already exists
		for _, existing := range repos.Additional {
			if existing == repoPath {
				return nil, fmt.Errorf("repository already configured: %s", repoPath)
			}
		}

		// Add the new repo
		repos.Additional = append(repos.Additional, repoPath)
		return repos, nil
	}, nil)
	return reposChangeErr(err)
}

// RemoveRepo removes a repository from the repos.additional list in config.yaml
func RemoveRepo(configPath, repoPath string) error {
	err := updateReposInYAML(configPath, func(repos *ReposConfig) (*ReposConfig, error) {
		// Find and remove the repo
		found := false
		newAdditional := make([]string, 0, len(repos.Additional))
		for _, existing := range repos.Additional {
			if existing == repoPath {
				found = true
				continue
			}
			newAdditional = append(newAdditional, existing)
		}

		if !found {
			return nil, fmt.Errorf("repository not found: %s", repoPath)
		}

		repos.Additional = newAdditional

		// If no repos left, clear primary too
		if len(repos.Additional) == 0 {
			repos.Primary = ""
		}
		return repos, nil
	}, nil)
	return reposChangeErr(err)
}

// ListRepos returns the current repos configuration from YAML
func ListRepos(configPath string) (*ReposConfig, error) {
	return GetReposFromYAML(configPath)
}
