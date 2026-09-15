package changes

// Darwin's O_SEARCH is O_EXEC | O_DIRECTORY (sys/fcntl.h). x/sys does not yet
// expose O_EXEC. Unlike O_EVTONLY, it requires search permission, not read
// permission, and its directory descriptors support fchmod and fsync.
const sourceSearchFlags = 0x40000000
const materializedDirectoryFlags = sourceSearchFlags
