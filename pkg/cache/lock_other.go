//go:build !unix

package cache

import "os"

// keeps is false where there is no lock to say a view is taken: every view
// is made new and removed when let go.
const keeps = false

func tryLock(*os.File) bool { return false }
