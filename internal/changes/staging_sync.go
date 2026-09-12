package changes

import "os"

// syncStagingBarrier completes the durability phase after syncStagedData on
// members of the same filesystem. On Darwin File.Sync requests F_FULLFSYNC,
// draining the device cache for preceding member fsyncs. Other systems use
// their normal fsync semantics. Finish this before exposing prepared data under
// durable names, then sync the containing directory again after those renames.
func syncStagingBarrier(file *os.File) error { return file.Sync() }
