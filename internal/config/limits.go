package config

// RequestBodyLimit returns the max request body size in bytes, derived from
// the configured MaxRequestBodyMB. A non-positive value falls back to 200 MB
// (vision requests routinely carry multi-MB base64 images).
func (c *Config) RequestBodyLimit() int64 {
	if c == nil || c.MaxRequestBodyMB <= 0 {
		return 200 << 20 // 200 MB fallback
	}
	return int64(c.MaxRequestBodyMB) * 1024 * 1024
}
