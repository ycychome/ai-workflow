// Package policies defines trusted, bounded verification profiles.
// Issue text and candidate files cannot provide commands or mandatory obligations.
package policies

import (
	"embed"
	"errors"
)

const FileImage = "alpine@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8"
const ServiceImage = "python@sha256:2d9aefe2fef018a7eb2c13064c89c71929800fd2e5dccdbf52ea5da5bb8d929a"
const FileCommand = `test "$(cat greeting.txt)" = 'hello M0'; test ! -e /var/run/docker.sock; test ! -e /run/secrets; test ! -e /root/.ssh; test -z "${GITHUB_TOKEN:-}"; test -z "${GITHUB_APP_PRIVATE_KEY:-}"; test -z "${SSH_AUTH_SOCK:-}"`

//go:embed greeting_acceptance.py
var scripts embed.FS

type Profile struct {
	Name          string   `json:"name"`
	SnapshotFiles []string `json:"snapshot_files"`
	Required      []string `json:"required"`
	Command       string   `json:"command"`
	DefaultImage  string   `json:"default_image"`
}

func Get(name string) (Profile, error) {
	switch name {
	case "", "greeting-file":
		return Profile{Name: "greeting-file", SnapshotFiles: []string{"greeting.txt", "README.md"}, Required: []string{"AC-01", "SEC-01"}, Command: FileCommand, DefaultImage: FileImage}, nil
	case "greeting-service":
		script, err := scripts.ReadFile("greeting_acceptance.py")
		if err != nil {
			return Profile{}, err
		}
		return Profile{Name: name, SnapshotFiles: []string{"greeting.txt", "README.md", "app.py", "test_app.py"}, Required: []string{"AC-01", "SEC-01", "HTTP-01", "HTTP-02", "COMP-01"}, Command: FileCommand + "\npython3 -I -B - <<'M0_TRUSTED_ACCEPTANCE'\n" + string(script) + "\nM0_TRUSTED_ACCEPTANCE", DefaultImage: ServiceImage}, nil
	default:
		return Profile{}, errors.New("unknown trusted verification profile")
	}
}

func SnapshotPath(name string) bool {
	return name == "greeting.txt" || name == "README.md" || name == "app.py" || name == "test_app.py"
}
