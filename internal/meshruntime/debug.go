package meshruntime

import (
	"log"
	"os"
)

func debugMeshf(format string, args ...any) {
	if os.Getenv("PARKER_DEBUG_MESH") == "" {
		return
	}
	log.Printf("[mesh-debug] "+format, args...)
}
