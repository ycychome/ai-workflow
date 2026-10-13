package sandbox

import (
	"errors"
	"io/fs"
)

// NormalizeGitFileMode maps ordinary regular-file permissions to Git's two
// tracked modes. Git records only the owner execute bit; read/write bits and
// group/other execute bits do not affect the tree. File type and special bits
// are rejected instead of silently discarded.
func NormalizeGitFileMode(mode fs.FileMode) (fs.FileMode, error) {
	if mode&^fs.ModePerm != 0 {
		return 0, errors.New("file mode contains a type or special bit")
	}
	if mode&0100 != 0 {
		return 0755, nil
	}
	return 0644, nil
}
