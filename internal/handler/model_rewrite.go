// model_rewrite.go: extraction and rewriting of the `model` field in
// inbound request bodies. Supports both JSON requests (the common case) and
// multipart/form-data (Whisper-style audio endpoints). The proxy accepts
// client-facing model names and may need to rewrite them to a backend-
// specific ID before forwarding; these helpers do that translation
// without re-parsing the full body.
package handler

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"strings"
)

// maxModelFieldBytes caps how much we will read from a multipart "model"
// field. Model names are short (dozens of characters); a 1 KB cap is ~50x
// headroom for anything legitimate and protects against a client sending
// a 50 MB "model" field to OOM the parser.
const maxModelFieldBytes = 1024

// ExtractModelFromJSON pulls the model name from a JSON request body.
func ExtractModelFromJSON(body []byte) string {
	var req struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &req) == nil {
		return req.Model
	}
	return ""
}

// ExtractModelFromMultipart pulls the model name from a multipart/form-data body.
// The model field is capped at maxModelFieldBytes; anything longer is treated
// as an invalid request (empty return).
func ExtractModelFromMultipart(body []byte, contentType string) string {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ""
	}
	boundary := params["boundary"]
	if boundary == "" {
		return ""
	}

	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := reader.NextPart()
		if err != nil {
			break
		}
		if part.FormName() == "model" {
			val, err := io.ReadAll(io.LimitReader(part, maxModelFieldBytes+1))
			part.Close()
			if err != nil || len(val) > maxModelFieldBytes {
				return ""
			}
			return strings.TrimSpace(string(val))
		}
		part.Close()
	}
	return ""
}

// RewriteModelName replaces the "model" field in a JSON body. Other field values
// are preserved as raw bytes via json.RawMessage, but top-level key order may change.
func RewriteModelName(body []byte, newName string) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	if _, ok := m["model"]; !ok {
		return body
	}
	nameBytes, err := json.Marshal(newName)
	if err != nil {
		return body
	}
	m["model"] = json.RawMessage(nameBytes)
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// RewriteModelInMultipart rebuilds a multipart body with the model field replaced.
func RewriteModelInMultipart(body []byte, contentType string, newModel string) []byte {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return body
	}
	boundary := params["boundary"]
	if boundary == "" {
		return body
	}

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	writer.SetBoundary(boundary) // preserve original boundary so Content-Type header stays valid

	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := reader.NextPart()
		if err != nil {
			break
		}

		if part.FormName() == "model" {
			// Replace model value.
			fw, err := writer.CreateFormField("model")
			if err != nil {
				part.Close()
				return body
			}
			if _, err := fw.Write([]byte(newModel)); err != nil {
				part.Close()
				return body
			}
			part.Close()
			continue
		}

		// Copy other parts as-is.
		header := part.Header
		pw, err := writer.CreatePart(header)
		if err != nil {
			part.Close()
			return body
		}
		if _, err := io.Copy(pw, part); err != nil {
			part.Close()
			return body
		}
		part.Close()
	}
	writer.Close()
	return buf.Bytes()
}
