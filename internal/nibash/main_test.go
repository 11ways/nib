package nibash

import (
	"os"
	"testing"

	"github.com/11ways/nib/internal/trash"
)

func TestMain(m *testing.M) {
	cleanup := trash.Isolated()
	status := m.Run()
	cleanup()
	os.Exit(status)
}
