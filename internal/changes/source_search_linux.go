package changes

import "golang.org/x/sys/unix"

const sourceSearchFlags = unix.O_PATH

// O_PATH descriptors cannot be synchronized. Destination directories normally
// retain read access until their children have been synchronized.
const materializedDirectoryFlags = unix.O_RDONLY
