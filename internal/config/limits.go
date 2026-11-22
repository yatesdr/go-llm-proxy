package config

// DefaultMaxRequestBodyMB is the request body limit when max_request_body_mb
// is unset. Each request body is read whole into memory (and copied while it
// is translated), so the limit bounds the memory one request can pin.
const DefaultMaxRequestBodyMB = 50

// MaxRequestBodyMBCeiling is the largest max_request_body_mb accepted.
const MaxRequestBodyMBCeiling = 2048

// RequestBodyLimit is the request body limit in bytes: max_request_body_mb,
// or DefaultMaxRequestBodyMB when it is unset.
func (c *Config) RequestBodyLimit() int64 {
	mb := DefaultMaxRequestBodyMB
	if c != nil && c.MaxRequestBodyMB > 0 {
		mb = c.MaxRequestBodyMB
	}
	return int64(mb) << 20
}

// MaxKeyConcurrentCeiling is the largest per-key max_concurrent accepted.
const MaxKeyConcurrentCeiling = 1000
