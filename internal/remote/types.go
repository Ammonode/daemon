// Package remote talks to the panel's /api/remote endpoints.
package remote

// ServerConfiguration is everything the daemon needs to run one server. It is
// produced by the panel from the server, its egg and its variables.
type ServerConfiguration struct {
	UUID      string `json:"uuid"`
	Suspended bool   `json:"suspended"`
	// Startup command, still containing {{VARIABLE}} placeholders; the image
	// entrypoint expands them from the environment (same as Pterodactyl yolks).
	Invocation  string            `json:"invocation"`
	SkipScripts bool              `json:"skip_egg_scripts"`
	Environment map[string]string `json:"environment"`
	Labels      map[string]string `json:"labels"`
	Build       Build             `json:"build"`
	Container   struct {
		Image string `json:"image"`
	} `json:"container"`
	Allocations Allocations `json:"allocations"`
	Egg         struct {
		ID           string   `json:"id"`
		FileDenylist []string `json:"file_denylist"`
		Features     []string `json:"features"`
	} `json:"egg"`
	Mounts       []Mount       `json:"mounts"`
	Process      ProcessConfig `json:"process_configuration"`
	CrashRestart bool          `json:"crash_restart"`
}

type Build struct {
	MemoryLimit int64  `json:"memory_limit"` // MiB
	Swap        int64  `json:"swap"`         // MiB, -1 unlimited
	IOWeight    uint16 `json:"io_weight"`
	CPULimit    int64  `json:"cpu_limit"` // percent, 0 unlimited
	Threads     string `json:"threads"`
	DiskSpace   int64  `json:"disk_space"` // MiB, 0 unlimited
	OOMDisabled bool   `json:"oom_disabled"`
}

type Allocation struct {
	IP   string `json:"ip"`
	Port int    `json:"port"`
}

type Allocations struct {
	ForceOutgoingIP bool             `json:"force_outgoing_ip"`
	Default         Allocation       `json:"default"`
	Mappings        map[string][]int `json:"mappings"`
}

type Mount struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
}

type ProcessConfig struct {
	Startup struct {
		Done            []string `json:"done"`
		UserInteraction []string `json:"user_interaction"`
		StripAnsi       bool     `json:"strip_ansi"`
	} `json:"startup"`
	Stop struct {
		// command | signal | stop (docker stop)
		Type  string `json:"type"`
		Value string `json:"value"`
	} `json:"stop"`
	Configs []ConfigFile `json:"configs"`
}

// ConfigFile mirrors an entry of the egg's `config.files`.
type ConfigFile struct {
	File    string              `json:"file"`
	Parser  string              `json:"parser"` // properties | json | yaml | ini | xml | file
	Replace []ConfigReplacement `json:"replace"`
}

type ConfigReplacement struct {
	Match   string      `json:"match"`
	IfValue string      `json:"if_value,omitempty"`
	Value   interface{} `json:"replace_with"`
}

type InstallationScript struct {
	ContainerImage string `json:"container_image"`
	Entrypoint     string `json:"entrypoint"`
	Script         string `json:"script"`
}

type SftpAuthRequest struct {
	Type          string `json:"type"` // password | public_key
	User          string `json:"username"`
	Pass          string `json:"password"`
	IP            string `json:"ip"`
	SessionID     []byte `json:"session_id"`
	ClientVersion []byte `json:"client_version"`
}

type SftpAuthResponse struct {
	Server      string   `json:"server"`
	User        string   `json:"user"`
	Permissions []string `json:"permissions"`
}

type ActivityEvent struct {
	Server    string                 `json:"server"`
	Event     string                 `json:"event"`
	User      string                 `json:"user,omitempty"`
	IP        string                 `json:"ip,omitempty"`
	Metadata  map[string]interface{} `json:"metadata"`
	Timestamp string                 `json:"timestamp"`
}
