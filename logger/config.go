package logger

// Config is the logger configuration (simplified version)
type Config struct {
	Level     string `json:"level"`       // Log level: debug, info, warn, error (default: info)
	LogDir    string `json:"log_dir"`     // Log directory (default: data)
	MaxSizeMB int64  `json:"max_size_mb"` // Rotate active file at this size (default: 100 MiB)
}

// SetDefaults sets default values
func (c *Config) SetDefaults() {
	if c.Level == "" {
		c.Level = "info"
	}
	if c.LogDir == "" {
		c.LogDir = "data"
	}
	if c.MaxSizeMB <= 0 {
		c.MaxSizeMB = 100
	}
}
