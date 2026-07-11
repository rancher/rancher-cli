package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// LegacyPaths returns candidate cli2.json locations, in probe order.
// $RANCHER_CLI_CONFIG_DIR/cli2.json (if set) then always ~/.rancher/cli2.json.
func LegacyPaths() []string {
	var paths []string
	if dir := os.Getenv("RANCHER_CLI_CONFIG_DIR"); dir != "" {
		paths = append(paths, filepath.Join(dir, "cli2.json"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".rancher", "cli2.json"))
	}
	return paths
}

// FindLegacy returns the first existing legacy config path.
func FindLegacy() (string, bool) {
	for _, p := range LegacyPaths() {
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return "", false
}

// Migrate imports a legacy cli2.json into a new Config (written to newDir)
// and stores each server's token (accessKey:secretKey) in the credential store.
func Migrate(legacyPath, newDir string, store CredStore) (*Config, error) {
	b, err := os.ReadFile(legacyPath)
	if err != nil {
		return nil, err
	}
	var lc legacyConfig
	if err := json.Unmarshal(b, &lc); err != nil {
		return nil, err
	}

	c := &Config{
		CurrentContext: lc.CurrentServer,
		Contexts:       map[string]Context{},
		Servers:        map[string]Server{},
		path:           FilePath(newDir),
	}
	for name, s := range lc.Servers {
		if s == nil {
			continue
		}
		c.Servers[name] = Server{URL: s.URL, CA: s.CACerts}
		cluster, project, _ := SplitContext(s.Project)
		c.Contexts[name] = Context{Server: name, Cluster: cluster, Project: project}

		token := s.TokenKey
		if token == "" && s.AccessKey != "" {
			token = s.AccessKey + ":" + s.SecretKey
		}
		if token != "" {
			if err := store.SetToken(name, token); err != nil {
				return nil, err
			}
		}
	}
	if err := c.Write(); err != nil {
		return nil, err
	}
	return c, nil
}

// PurgeLegacy deletes a legacy config file.
func PurgeLegacy(path string) error {
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
