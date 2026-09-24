// Package buildinfo identifies the AIH binary that is actually executing.
// The Git revision comes from Go's supported VCS build metadata, not from a
// repository checkout that may advance while a supervisor remains running.
package buildinfo

import (
	"runtime/debug"
	"strconv"

	"github.com/nvrakesh06/ai-agentic-harness/internal/model"
)

type Identity struct {
	Version     string `json:"version"`
	StateSchema int    `json:"state_schema"`
	Commit      string `json:"commit,omitempty"`
	Dirty       bool   `json:"dirty,omitempty"`
}

func Current() Identity {
	id := Identity{Version: model.Version, StateSchema: model.StateSchema}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				id.Commit = setting.Value
			case "vcs.modified":
				id.Dirty = setting.Value == "true"
			}
		}
	}
	return id
}

func (id Identity) Label() string {
	commit := id.Commit
	if commit == "" {
		commit = "unknown commit"
	} else if len(commit) > 12 {
		commit = commit[:12]
	}
	if id.Dirty {
		commit += "+dirty"
	}
	return id.Version + " / " + commit + " / state " + strconv.Itoa(id.StateSchema)
}
