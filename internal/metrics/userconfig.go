package metrics

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/steveyegge/beads/internal/config"
)

var commentedMetricsRe = regexp.MustCompile(`(?m)^\s*#\s*metrics\s*:`)

// EnsureUserConfigDefaults adds the metrics defaults the user config lacks:
// the whole bootstrap body to an absent file, else metrics.disabled and
// metrics.endpoint where missing. It first reads the file under the shared
// config lock and decides; a complete file (or a commented-out metrics block)
// returns there, so a read-only file or directory is never opened for
// writing. Only when a key is missing does it take one locked rewrite, which
// re-reads the file and decides again under the exclusive lock, so a value
// another bd writes meanwhile (metrics.disabled: true, a custom endpoint) is
// never overwritten, and a concurrent bd never reads the file half-made.
func EnsureUserConfigDefaults() error {
	path, err := config.UserConfigYamlPath()
	if err != nil {
		return fmt.Errorf("ensure user config: %w", err)
	}

	// Decide from a shared-lock read. The lock is released before any write
	// lock is taken: a lock is never upgraded in place.
	data, err := config.ReadConfigFileShared(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Absent: the bootstrap below creates the file, and its directory.
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("ensure user config: mkdir %s: %w", filepath.Dir(path), err)
		}
	case err != nil:
		return fmt.Errorf("ensure user config: read %s: %w", path, err)
	default:
		if _, _, need, err := missingMetricsDefaults(path, data, true); err != nil || !need {
			return err
		}
	}

	body := []byte("metrics:\n  disabled: false\n  endpoint: " + DefaultEndpoint + "\n")
	step := "" // what the last edit wrote: "" (bootstrap) or the first key set
	err = config.RewriteConfigFile(path, 0o600, func(data []byte, exists bool) ([]byte, error) {
		step = ""
		if !exists {
			return body, nil
		}
		needDisabled, needEndpoint, need, err := missingMetricsDefaults(path, data, exists)
		if err != nil {
			return nil, err
		}
		if !need {
			return nil, config.ErrSkipRewrite
		}

		content := string(data)
		if needDisabled {
			step = "metrics.disabled"
			if content, err = config.UpdateYamlConfigContent(content, "metrics.disabled", "false"); err != nil {
				return nil, fmt.Errorf("ensure user config: set metrics.disabled: %w", err)
			}
		}
		if needEndpoint {
			if step == "" {
				step = "metrics.endpoint"
			}
			if content, err = config.UpdateYamlConfigContent(content, "metrics.endpoint", DefaultEndpoint); err != nil {
				return nil, fmt.Errorf("ensure user config: set metrics.endpoint: %w", err)
			}
		}
		return []byte(content), nil
	})
	if err == nil {
		return nil
	}
	op, cause := config.RewriteConfigFileCause(err)
	switch {
	case op == "":
		return err // the edit's own error, already worded
	case step != "":
		return fmt.Errorf("ensure user config: set %s: failed to %s config.yaml: %w", step, op, cause)
	default:
		return fmt.Errorf("ensure user config: %s %s: %w", op, path, cause)
	}
}

// missingMetricsDefaults reports which metrics defaults the user config's
// bytes lack. A commented-out metrics block means the user opted out of the
// defaults: nothing is missing. It reads no file, so a locked edit may call it.
func missingMetricsDefaults(path string, data []byte, exists bool) (needDisabled, needEndpoint, need bool, err error) {
	if !exists {
		return true, true, true, nil
	}
	if commentedMetricsRe.Match(data) {
		return false, false, false, nil
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return false, false, false, fmt.Errorf("ensure user config: parse %s: %w", path, err)
	}
	needDisabled = !userConfigHasLeaf(&root, "metrics", "disabled")
	needEndpoint = !userConfigHasLeaf(&root, "metrics", "endpoint")
	return needDisabled, needEndpoint, needDisabled || needEndpoint, nil
}

func userConfigHasLeaf(root *yaml.Node, parts ...string) bool {
	if root == nil || len(root.Content) == 0 {
		return false
	}
	mapping := root.Content[0]
	if mapping.Kind != yaml.MappingNode {
		return false
	}

	flatKey := strings.Join(parts, ".")
	for i := 0; i < len(mapping.Content); i += 2 {
		k := mapping.Content[i]
		if k.Kind == yaml.ScalarNode && k.Value == flatKey {
			v := mapping.Content[i+1]
			return v.Kind == yaml.ScalarNode && v.Value != ""
		}
	}

	current := mapping
	for _, part := range parts {
		if current.Kind != yaml.MappingNode {
			return false
		}
		idx := -1
		for i := 0; i < len(current.Content); i += 2 {
			k := current.Content[i]
			if k.Kind == yaml.ScalarNode && k.Value == part {
				idx = i
				break
			}
		}
		if idx == -1 {
			return false
		}
		current = current.Content[idx+1]
	}
	return current.Kind == yaml.ScalarNode && current.Value != ""
}
