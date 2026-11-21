//go:build !windows

package release

import "os"

func (disk) backup(path, backup string) error { return os.Link(path, backup) }
func (disk) replace(from, to string) error    { return os.Rename(from, to) }
