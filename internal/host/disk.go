package host

import "syscall"

// diskUsage reports used and total bytes for the filesystem holding path.
// Bsize is int64 on Linux and uint32 on Darwin, hence the explicit conversion.
func diskUsage(path string) (used, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bs := uint64(st.Bsize)
	total = st.Blocks * bs
	// Blocks reserved for root are unusable, so count them as used rather than
	// reporting free space nothing can actually claim.
	used = (st.Blocks - st.Bavail) * bs
	return used, total, nil
}

// deviceOf returns the id of the device holding path. Two configured paths on
// the same filesystem describe one volume, and reporting both produces two
// identical rows — /var/lib/ol1n-status sitting on / being the obvious case.
func deviceOf(path string) (uint64, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Dev), nil
}
