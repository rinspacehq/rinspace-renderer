package pdfinspect

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

// A separate process prevents an earlier PDF test from populating pdfcpu's
// global configuration cache and hiding an accidental dependency on HOME.
func TestInspectWithoutAmbientConfiguration(t *testing.T) {
	if os.Getenv("RINSPACE_PDFINSPECT_CONFIG_PROBE") == "1" {
		var workers sync.WaitGroup
		for range 12 {
			workers.Add(1)
			go func() {
				defer workers.Done()
				result, err := Inspect(validOnePagePDF())
				if err != nil || result.PageCount != 1 {
					t.Errorf("configuration-independent inspection failed: %#v, %v", result, err)
				}
				if _, err := Inspect(validOnePagePDFWithJavaScript()); !errors.Is(err, ErrQuarantined) {
					t.Errorf("active content was not quarantined: %v", err)
				}
			}()
		}
		workers.Wait()
		return
	}
	for _, mode := range []string{"unavailable", "hostile"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			config := filepath.Join(root, "config")
			if mode == "unavailable" {
				// A file parent cannot be written even by root, unlike chmod(0555).
				if err := os.WriteFile(config, []byte("not a directory"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.MkdirAll(filepath.Join(config, "pdfcpu"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(config, "pdfcpu", "config.yml"), []byte("invalid: [ambient configuration must not be read"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("HOME", filepath.Join(config, "home"))
			t.Setenv("XDG_CONFIG_HOME", config)
			t.Setenv("RINSPACE_PDFINSPECT_CONFIG_PROBE", "1")
			command := exec.Command(os.Args[0], "-test.run=^TestInspectWithoutAmbientConfiguration$", "-test.count=1")
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("fresh-process inspection with %s config failed: %v\n%s", mode, err, output)
			}
		})
	}
}

func TestInspectPreservesSourceIdentityAndPlansDerivatives(t *testing.T) {
	body := validOnePagePDF()
	result, err := Inspect(body)
	if err != nil {
		t.Fatal(err)
	}
	if result.ContentKind != "pdf" || result.Engine != "pdf-inspect" || result.PageCount != 1 || result.SourceBytes != int64(len(body)) || len(result.SourceSHA256) != 64 || result.OriginalReencoded || len(result.Derivatives) != 2 {
		t.Fatalf("unexpected PDF inspection: %#v", result)
	}
}

func validOnePagePDF() []byte {
	var body bytes.Buffer
	body.WriteString("%PDF-1.4\n")
	offsets := make([]int, 5)
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R >>",
		"<< /Length 0 >>\nstream\n\nendstream",
	}
	for index, object := range objects {
		offsets[index+1] = body.Len()
		fmt.Fprintf(&body, "%d 0 obj\n%s\nendobj\n", index+1, object)
	}
	xref := body.Len()
	body.WriteString("xref\n0 5\n0000000000 65535 f \n")
	for index := 1; index <= 4; index++ {
		fmt.Fprintf(&body, "%010d 00000 n \n", offsets[index])
	}
	fmt.Fprintf(&body, "trailer\n<< /Size 5 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", xref)
	return body.Bytes()
}

func TestInspectQuarantinesActiveContentAndDamage(t *testing.T) {
	malicious := validOnePagePDFWithJavaScript()
	if _, err := Inspect(malicious); !errors.Is(err, ErrQuarantined) {
		t.Fatalf("active PDF was not quarantined: %v", err)
	}
	if _, err := Inspect([]byte("%PDF-1.7 damaged")); err == nil {
		t.Fatal("damaged PDF was accepted")
	}
}

func validOnePagePDFWithJavaScript() []byte {
	var body bytes.Buffer
	body.WriteString("%PDF-1.4\n")
	offsets := make([]int, 6)
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R /OpenAction 5 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R >>",
		"<< /Length 0 >>\nstream\n\nendstream",
		"<< /S /JavaScript /JS (app.alert\\(1\\)) >>",
	}
	for index, object := range objects {
		offsets[index+1] = body.Len()
		fmt.Fprintf(&body, "%d 0 obj\n%s\nendobj\n", index+1, object)
	}
	xref := body.Len()
	body.WriteString("xref\n0 6\n0000000000 65535 f \n")
	for index := 1; index <= 5; index++ {
		fmt.Fprintf(&body, "%010d 00000 n \n", offsets[index])
	}
	fmt.Fprintf(&body, "trailer\n<< /Size 6 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", xref)
	return body.Bytes()
}
