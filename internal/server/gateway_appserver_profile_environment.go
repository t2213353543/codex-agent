package server

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"strings"
)

// appServerProfileEnvironment is evidence extracted from the client's latest
// complete environment_context. Empty fields remain unknown; host OS/cwd are
// never consulted and this object does not grant execution capability.
type appServerProfileEnvironment struct {
	CWD, Shell, CurrentDate, Timezone string
	WorkspaceRoots                    []string
	SandboxMode                       string
	MultiEnvironment                  bool
}

func appServerEnvironmentFromInput(items []json.RawMessage) (*appServerProfileEnvironment, error) {
	var latest *appServerProfileEnvironment
	for _, raw := range items {
		var item struct {
			Role    string
			Content json.RawMessage
		}
		if json.Unmarshal(raw, &item) != nil || item.Role != "user" {
			continue
		}
		var parts []struct{ Type, Text string }
		var text string
		if json.Unmarshal(item.Content, &text) == nil {
			parts = append(parts, struct{ Type, Text string }{"input_text", text})
		} else if json.Unmarshal(item.Content, &parts) != nil {
			continue
		}
		for _, part := range parts {
			text := strings.TrimSpace(part.Text)
			if part.Type != "input_text" || !strings.HasPrefix(text, "<environment_context>") || !strings.HasSuffix(text, "</environment_context>") {
				continue
			}
			var value struct {
				XMLName      xml.Name  `xml:"environment_context"`
				CWD          string    `xml:"cwd"`
				Shell        string    `xml:"shell"`
				CurrentDate  string    `xml:"current_date"`
				Timezone     string    `xml:"timezone"`
				Environments *struct{} `xml:"environments"`
				Filesystem   struct {
					Roots   []string `xml:"workspace_roots>root"`
					Profile struct {
						Type       string `xml:"type,attr"`
						FileSystem struct {
							Type    string `xml:"type,attr"`
							Entries []struct {
								Access  string `xml:"access,attr"`
								Special string `xml:"special"`
								Path    string `xml:"path"`
							} `xml:"entry"`
						} `xml:"file_system"`
					} `xml:"permission_profile"`
				} `xml:"filesystem"`
			}
			if xml.Unmarshal([]byte(text), &value) != nil {
				return nil, errors.New("invalid client environment_context XML")
			}
			result := &appServerProfileEnvironment{CWD: value.CWD, Shell: value.Shell, CurrentDate: value.CurrentDate, Timezone: value.Timezone, WorkspaceRoots: append([]string(nil), value.Filesystem.Roots...), MultiEnvironment: value.Environments != nil}
			profile := value.Filesystem.Profile
			if profile.Type == "disabled" && profile.FileSystem.Type == "unrestricted" {
				result.SandboxMode = "danger-full-access"
			}
			if profile.Type == "managed" && profile.FileSystem.Type == "restricted" && len(profile.FileSystem.Entries) > 0 {
				readOnly, rootReadable := true, false
				for _, entry := range profile.FileSystem.Entries {
					if entry.Access != "read" && entry.Access != "deny" {
						readOnly = false
					}
					if entry.Access == "read" && entry.Special == ":root" {
						rootReadable = true
					}
				}
				if readOnly && rootReadable {
					result.SandboxMode = "read-only"
				}
			}
			latest = result
		}
	}
	return latest, nil
}

// contextProfile combines proven client environment fields with explicitly
// supplied approval/network/collaboration/tool policy. It does not infer those
// policy settings from the gateway host or inject a multi-agent role itself.
func (e *appServerProfileEnvironment) contextProfile(explicit appServerContextProfile) (appServerContextProfile, error) {
	if e == nil || e.CWD == "" || e.MultiEnvironment {
		return explicit, errors.New("initial context requires a complete single client environment")
	}
	if e.SandboxMode == "" {
		return explicit, errors.New("client environment sandbox profile is not established")
	}
	if explicit.SandboxMode != "" && explicit.SandboxMode != e.SandboxMode {
		return explicit, errors.New("client environment conflicts with explicit sandbox profile")
	}
	explicit.SandboxMode = e.SandboxMode
	explicit.CWD, explicit.Shell, explicit.CurrentDate, explicit.Timezone = e.CWD, e.Shell, e.CurrentDate, e.Timezone
	explicit.WorkspaceRoots = append([]string(nil), e.WorkspaceRoots...)
	if explicit.ApprovalPolicy == "" || explicit.NetworkAccess == "" {
		return explicit, errors.New("client approval and network policy are required for context rendering")
	}
	return explicit, nil
}
