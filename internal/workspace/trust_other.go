//go:build !darwin && !linux

package workspace

import "os"

func ownedByCurrentUser(os.FileInfo) bool {
	return false
}
