package core

import (
	"errors"
	"regexp"
	"time"
)

var ContainerID = regexp.MustCompile(`^[a-f0-9]{64}$`)

// Only these metadata fields leave the NAS. Environment, mounts and arbitrary
// Docker labels are never included in a discovery report.
type DockerApplication struct {
	ID           string `json:"application_id"`
	ContainerID  string `json:"container_id"`
	Name         string `json:"name"`
	IdentityKind string `json:"identity_kind"`
	State        string `json:"state"`
	Running      bool   `json:"running"`
	TargetKey    string `json:"target_key"`
	TargetURL    string `json:"target_url,omitempty"`
	EntryPath    string `json:"entry_path"`
}

type DockerReport struct {
	Schema       int                 `json:"schema_version"`
	State        string              `json:"state"`
	ScannedAt    int64               `json:"scanned_at"`
	Applications []DockerApplication `json:"applications"`
}

func (r DockerReport) Validate() error {
	if r.Schema != 1 || len(r.Applications) > 256 || r.ScannedAt < 0 || r.ScannedAt > time.Now().Unix()+120 {
		return errors.New("invalid docker report")
	}
	switch r.State {
	case "ready", "unavailable", "disabled":
	default:
		return errors.New("invalid discovery state")
	}
	ids, containers, keys := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, a := range r.Applications {
		if !Identifier.MatchString(a.ID) || !ContainerID.MatchString(a.ContainerID) || len(a.Name) == 0 || len(a.Name) > 256 || !Identifier.MatchString(a.TargetKey) || !SafePath(a.EntryPath) || len(a.EntryPath) > 2048 || ids[a.ID] || containers[a.ContainerID] || keys[a.TargetKey] {
			return errors.New("invalid docker application")
		}
		ids[a.ID], containers[a.ContainerID], keys[a.TargetKey] = true, true, true
		switch a.IdentityKind {
		case "compose", "unraid_template", "container_name":
		default:
			return errors.New("invalid application identity")
		}
		switch a.State {
		case "ready", "stopped", "no_webui", "invalid_webui", "network_unresolved", "identity_conflict":
		default:
			return errors.New("invalid application state")
		}
		if a.TargetURL != "" && (Target{Key: a.TargetKey, Name: a.Name, URL: a.TargetURL}).Validate() != nil {
			return errors.New("invalid docker target")
		}
		if a.State == "ready" && (!a.Running || a.TargetURL == "") {
			return errors.New("ready application without target")
		}
	}
	return nil
}

func (r *DockerReport) Fresh() bool {
	return r != nil && r.State == "ready" && r.ScannedAt > 0 && time.Now().Unix()-r.ScannedAt < 90
}

func (r *DockerReport) Available(key string) bool {
	if !r.Fresh() {
		return false
	}
	for _, a := range r.Applications {
		if a.TargetKey == key {
			return a.State == "ready" && a.Running
		}
	}
	return false
}
