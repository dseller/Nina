package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// Load reads and validates a configuration file. Dir is retained so relative
// spec `file` paths resolve against the config's own location rather than the
// process working directory.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	c, err := Parse(filepath.Base(path), data)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	c.Path = abs
	c.Dir = filepath.Dir(abs)
	return c, nil
}

// ResolvePath resolves p relative to the configuration file's directory.
func (c *Config) ResolvePath(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	if c.Dir == "" {
		return p
	}
	return filepath.Join(c.Dir, p)
}

// BackendByName returns the named backend, or nil.
func (c *Config) BackendByName(name string) *Backend {
	for i := range c.Backends {
		if c.Backends[i].Name == name {
			return &c.Backends[i]
		}
	}
	return nil
}

// WatchFiles lists every local file whose change should trigger a reload.
func (c *Config) WatchFiles() []string {
	out := []string{}
	if c.Path != "" {
		out = append(out, c.Path)
	}
	for i := range c.Backends {
		if f := c.Backends[i].Spec.File; f != "" {
			out = append(out, c.ResolvePath(f))
		}
	}
	return out
}
