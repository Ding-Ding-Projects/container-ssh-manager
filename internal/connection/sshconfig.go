package connection

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"
)

// SSHConfigImport is a deliberately small, reviewable import format. Secrets
// and IdentityFile contents are never read by the importer.
type SSHConfigImport struct {
	Hosts       []HostInput          `json:"hosts"`
	Unsupported []SSHConfigDirective `json:"unsupported"`
}
type SSHConfigDirective struct {
	Line   int    `json:"line"`
	Name   string `json:"name"`
	Value  string `json:"value"`
	Reason string `json:"reason"`
}
type HostInput struct {
	Name         string   `json:"name"`
	Address      string   `json:"address"`
	Port         int      `json:"port"`
	User         string   `json:"user"`
	CredentialID string   `json:"credentialId"`
	JumpIDs      []string `json:"jumpIds"`
	Group        string   `json:"group"`
	Tags         []string `json:"tags"`
}

func ParseSSHConfig(text string) SSHConfigImport {
	var out SSHConfigImport
	var current *HostInput
	for lineNo, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(strings.SplitN(raw, "#", 2)[0])
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			out.Unsupported = append(out.Unsupported, SSHConfigDirective{Line: lineNo + 1, Name: line, Reason: "directive needs a value"})
			continue
		}
		key, val := strings.ToLower(fields[0]), strings.Join(fields[1:], " ")
		if key == "host" {
			for _, name := range fields[1:] {
				if strings.ContainsAny(name, "*?!") {
					out.Unsupported = append(out.Unsupported, SSHConfigDirective{Line: lineNo + 1, Name: "Host", Value: name, Reason: "patterns are not imported"})
					continue
				}
				out.Hosts = append(out.Hosts, HostInput{Name: name, Port: 22})
				current = &out.Hosts[len(out.Hosts)-1]
			}
			continue
		}
		if current == nil {
			out.Unsupported = append(out.Unsupported, SSHConfigDirective{Line: lineNo + 1, Name: fields[0], Value: val, Reason: "directive precedes Host"})
			continue
		}
		switch key {
		case "hostname":
			current.Address = val
		case "port":
			p, e := strconv.Atoi(val)
			if e != nil || p < 1 || p > 65535 {
				out.Unsupported = append(out.Unsupported, SSHConfigDirective{Line: lineNo + 1, Name: fields[0], Value: val, Reason: "invalid port"})
			} else {
				current.Port = p
			}
		case "user":
			current.User = val
		case "proxyjump":
			for _, hop := range strings.Split(val, ",") {
				hop = strings.TrimSpace(hop)
				if hop != "" {
					current.JumpIDs = append(current.JumpIDs, hop)
				}
			}
		default:
			out.Unsupported = append(out.Unsupported, SSHConfigDirective{Line: lineNo + 1, Name: fields[0], Value: val, Reason: "unsupported directive"})
		}
	}
	for i := range out.Hosts {
		if out.Hosts[i].Address == "" {
			out.Hosts[i].Address = out.Hosts[i].Name
		}
	}
	return out
}
func ParseSSHConfigReader(r *bufio.Scanner) SSHConfigImport {
	var b strings.Builder
	for r.Scan() {
		fmt.Fprintln(&b, r.Text())
	}
	return ParseSSHConfig(b.String())
}
