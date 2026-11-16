//go:build unix

package cache

import (
	"os"
	"syscall"
)

// keeps says views are kept between runs: there is a lock to say one is
// taken.
const keeps = true

// tryLock takes the lock on f for whoever holds f open, this process or
// one that inherited it, without waiting. It reports whether it got it.
func tryLock(f *os.File) bool {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil
}
