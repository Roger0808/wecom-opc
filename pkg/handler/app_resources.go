package handler

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"weknora-tokendance/config"
	"weknora-tokendance/pkg/model"
)

var appEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var appSensitiveKey = regexp.MustCompile(`(?i)password|passwd|secret|token|dsn|database_url|connection|credential|api_key|access_key`)
var appSensitiveAssignment = regexp.MustCompile(`(?i)(password|passwd|secret|token|dsn|api[_-]?key|access[_-]?key)[^\s:=]*\s*[:=]\s*\S+`)

func redactAppRuntimeLogs(logs string, env []model.AppConfigItem) string {
	for _, item := range env {
		if item.Value != "" && appSensitiveKey.MatchString(item.Key) {
			logs = strings.ReplaceAll(logs, item.Value, "[REDACTED]")
		}
	}
	lines := strings.Split(logs, "\n")
	for i, line := range lines {
		if appSensitiveAssignment.MatchString(line) {
			lines[i] = "[REDACTED sensitive log line]"
		}
	}
	return strings.Map(func(r rune) rune {
		if r < 32 && r != '\n' && r != '\t' && r != '\r' {
			return -1
		}
		return r
	}, strings.Join(lines, "\n"))
}

func mergeAppEnvironment(resources []config.OnlineResourceConfig, ids []string, configs []model.AppConfigItem) ([]model.AppConfigItem, error) {
	available := make(map[string]config.OnlineResourceConfig)
	for _, resource := range resources {
		available[resource.ID] = resource
	}
	values := make(map[string]string)
	seenResources := make(map[string]bool)
	for _, id := range ids {
		resource, ok := available[id]
		if !ok || seenResources[id] {
			return nil, fmt.Errorf("unknown or duplicate resource: %s", id)
		}
		seenResources[id] = true
		for key, value := range resource.EnvVars {
			if !appEnvName.MatchString(key) {
				return nil, fmt.Errorf("invalid resource environment key")
			}
			if _, exists := values[key]; exists {
				return nil, fmt.Errorf("selected resources have conflicting key: %s", key)
			}
			values[key] = value
		}
	}
	seenConfigs := make(map[string]bool)
	for _, item := range configs {
		if !appEnvName.MatchString(item.Key) || seenConfigs[item.Key] {
			return nil, fmt.Errorf("invalid or duplicate application key: %s", item.Key)
		}
		seenConfigs[item.Key] = true
		// Blank UI fields represent server-side resource injection, not an instruction to erase it.
		if item.Value != "" || values[item.Key] == "" {
			values[item.Key] = item.Value
		}
		if item.Required && values[item.Key] == "" {
			return nil, fmt.Errorf("missing required configuration: %s", item.Key)
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]model.AppConfigItem, 0, len(keys))
	for _, key := range keys {
		out = append(out, model.AppConfigItem{Key: key, Value: values[key]})
	}
	return out, nil
}

func resourcePreview(env map[string]string) map[string]string {
	out := make(map[string]string, len(env))
	for key, value := range env {
		lower := strings.ToLower(key + " " + value)
		sensitive := strings.Contains(value, "@") || strings.Contains(value, "-----BEGIN")
		for _, word := range []string{"password", "passwd", "secret", "token", "api_key", "access_key", "dsn", "connectionstring", "connection_string"} {
			sensitive = sensitive || strings.Contains(lower, word)
		}
		if sensitive {
			out[key] = "[REDACTED]"
		} else {
			out[key] = value
		}
	}
	return out
}
