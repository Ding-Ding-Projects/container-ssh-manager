package engine

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// translatePayload keeps the public facade stable while matching Docker's wire
// format. Container Config fields belong at the top level of a create request.
func translatePayload(parts []string, method, path string, body []byte) (string, []byte, error) {
	if method != http.MethodPost || len(body) == 0 {
		return path, body, nil
	}
	var values map[string]json.RawMessage
	if json.Unmarshal(body, &values) != nil || values == nil {
		return path, nil, fmt.Errorf("request body must be a JSON object")
	}
	u, _ := url.Parse(path)
	q := u.Query()
	if parts[0] == "containers" && len(parts) == 1 {
		var config map[string]json.RawMessage
		raw, ok := values["config"]
		if !ok {
			raw, ok = values["Config"]
		}
		if ok {
			if json.Unmarshal(raw, &config) != nil || config == nil {
				return path, nil, fmt.Errorf("config must be an object")
			}
			delete(values, "config")
			delete(values, "Config")
			for k, v := range config {
				values[k] = v
			}
		}
		if raw, ok := values["name"]; ok {
			var name string
			if json.Unmarshal(raw, &name) != nil || !safeName(name) {
				return path, nil, fmt.Errorf("invalid container name")
			}
			q.Set("name", name)
			delete(values, "name")
		}
		for _, pair := range [][2]string{{"hostConfig", "HostConfig"}, {"networkingConfig", "NetworkingConfig"}} {
			if v, ok := values[pair[0]]; ok {
				values[pair[1]] = v
				delete(values, pair[0])
			}
		}
	}
	if parts[0] == "containers" && len(parts) == 3 && (parts[2] == "stop" || parts[2] == "restart") {
		if raw, ok := values["timeoutSeconds"]; ok {
			var seconds int
			if json.Unmarshal(raw, &seconds) != nil || seconds < 0 || seconds > 300 {
				return path, nil, fmt.Errorf("timeoutSeconds must be between 0 and 300")
			}
			q.Set("t", strconv.Itoa(seconds))
		}
		values = nil
	}
	if parts[0] == "images" && len(parts) == 3 && parts[2] == "tag" {
		var repository, tag string
		if json.Unmarshal(values["repository"], &repository) != nil || !safeBuildToken(repository) {
			return path, nil, fmt.Errorf("repository is required")
		}
		if raw, ok := values["tag"]; ok {
			if json.Unmarshal(raw, &tag) != nil || !safeBuildToken(tag) {
				return path, nil, fmt.Errorf("invalid tag")
			}
		}
		q.Set("repo", repository)
		if tag != "" {
			q.Set("tag", tag)
		}
		values = nil
	}
	u.RawQuery = q.Encode()
	if values == nil {
		return u.String(), nil, nil
	}
	encoded, err := json.Marshal(values)
	return u.String(), encoded, err
}
