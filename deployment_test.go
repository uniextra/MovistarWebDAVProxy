package deployment_test

import (
	"os"
	"strings"
	"testing"
)

// Evita que los ejemplos read_only vuelvan a enviar el spool completo a tmpfs.
func TestDiskSpoolDeploymentExamples(t *testing.T) {
	for _, file := range []string{"docker-compose.yml", "README.md"} {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, required := range []string{"WEBDAV_TMPDIR=/app/spool", "movistar_spool:/app/spool", "movistar_spool:"} {
			if !strings.Contains(string(data), required) {
				t.Errorf("%s: falta configuración de spool en disco %q", file, required)
			}
		}
	}
}
